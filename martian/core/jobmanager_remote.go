// Copyright (c) 2020 10X Genomics, Inc. All rights reserved.

package core

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path"
	"runtime/trace"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/martian-lang/martian/martian/util"
)

type RemoteJobManager struct {
	jobMode              string
	jobResourcesMappings map[string]string
	jobSem               *MaxJobsSemaphore
	limiter              *time.Ticker
	config               jobManagerConfig
	memGBPerCore         int
	maxJobs              int
	jobFreqMillis        int
	queueMutex           sync.Mutex
	debug                bool
}

func NewRemoteJobManager(jobMode string, memGBPerCore int, maxJobs int, jobFreqMillis int,
	jobResources string, config *JobManagerJson, debug bool) *RemoteJobManager {
	self := &RemoteJobManager{}
	self.jobMode = jobMode
	self.memGBPerCore = memGBPerCore
	self.maxJobs = maxJobs
	self.jobFreqMillis = jobFreqMillis
	self.debug = debug
	self.config = verifyJobManager(jobMode, config, memGBPerCore)

	// Parse jobresources mappings
	self.jobResourcesMappings = map[string]string{}
	for _, mapping := range strings.Split(jobResources, ";") {
		if len(mapping) > 0 {
			parts := strings.Split(mapping, ":")
			if len(parts) == 2 {
				self.jobResourcesMappings[parts[0]] = parts[1]
				util.LogInfo("jobmngr", "Mapping %s to %s", parts[0], parts[1])
			} else {
				util.LogInfo("jobmngr", "Could not parse mapping: %s", mapping)
			}
		}
	}

	if self.maxJobs > 0 {
		self.jobSem = NewMaxJobsSemaphore(self.maxJobs)
	}
	if self.jobFreqMillis > 0 {
		self.limiter = time.NewTicker(time.Millisecond * time.Duration(self.jobFreqMillis))
	} else {
		// dummy limiter to keep struct OK
		self.limiter = time.NewTicker(time.Millisecond * 1)
	}
	return self
}

func (self *RemoteJobManager) refreshResources(bool) error {
	if self.jobSem != nil {
		self.jobSem.FindDone()
	}
	return nil
}

func (self *RemoteJobManager) GetMaxCores() int {
	return 0
}

func (self *RemoteJobManager) GetMaxMemGB() int {
	return 0
}

func (self *RemoteJobManager) GetSettings() *JobManagerSettings {
	return self.config.jobSettings
}

func (self *RemoteJobManager) GetSystemReqs(resRequest *JobResources) JobResources {
	res := *resRequest
	// Sanity check the thread count.
	if res.Threads == 0 {
		res.Threads = float64(self.config.jobSettings.ThreadsPerJob)
	} else if res.Threads < 0 {
		res.Threads = -res.Threads
	}

	// Sanity check memory requirements.
	if res.MemGB < 0 {
		// Negative request is a sentinel value requesting as much as possible.
		// For remote jobs, at least for now, give reserve the minimum usable.
		res.MemGB = -res.MemGB
	}
	if res.MemGB == 0 {
		res.MemGB = float64(self.config.jobSettings.MemGBPerJob)
	}
	if res.VMemGB < 1 {
		res.VMemGB = res.MemGB + float64(self.config.jobSettings.ExtraVmemGB)
	}

	// Compute threads needed based on memory requirements.
	if self.memGBPerCore > 0 {
		if threadsForMemory := res.MemGB /
			float64(self.memGBPerCore); threadsForMemory > res.Threads {
			res.Threads = threadsForMemory
		}
	}

	// If threading is disabled, use only 1 thread.
	if !self.config.threadingEnabled {
		res.Threads = 1
	} else {
		// Remote job managers generally only support integer thread granularity.
		res.Threads = math.Ceil(res.Threads)
	}

	return res
}

func (self *RemoteJobManager) execJob(shellCmd string, argv []string,
	envs map[string]string, metadata *Metadata, resRequest *JobResources,
	fqname string, shellName string, localpreflight bool) {
	ctx, task := trace.NewTask(context.Background(), "queueRemote")

	// no limit, send the job
	if self.maxJobs <= 0 {
		defer task.End()
		self.sendJob(shellCmd, argv, envs,
			metadata, resRequest,
			fqname, shellName, ctx)
		return
	}

	// grab job when ready.  MaxJobsSemaphore takes care of polling for job
	// completion.
	// Pass in self.jobSem to the goroutine rather than using self.jobSem in
	// the goroutine to avoid a potential race if the jobSem is replaced during
	// an auto-restart.
	go func(ctx context.Context, task *trace.Task, jobSem *MaxJobsSemaphore) {
		defer task.End()
		if self.debug {
			util.LogInfo("jobmngr", "Waiting for job: %s", fqname)
		}
		// if we want to try to put a more precise cap on cluster execution load,
		// might be preferable to request num threads here instead of a slot per job
		if success := jobSem.Acquire(metadata, false); !success {
			if self.debug {
				util.LogInfo("jobmngr",
					"Wait for job %s canceled.",
					fqname)
			}
			return
		}
		if self.debug {
			util.LogInfo("jobmngr", "Job sent: %s", fqname)
		}
		self.sendJob(shellCmd, argv, envs,
			metadata, resRequest,
			fqname, shellName, ctx)
	}(ctx, task, self.jobSem)
}

func (self *RemoteJobManager) endJob(metadata *Metadata) {
	if self.jobSem != nil {
		self.jobSem.Release(metadata)
	}
}

func (self *RemoteJobManager) jobScript(
	shellCmd string, argv []string, envs map[string]string,
	metadata *Metadata,
	resRequest *JobResources,
	fqname, shellName string) string {
	res := self.GetSystemReqs(resRequest)

	// figure out per-thread memory requirements for the template.
	// ceil to make sure that we're not starving a job.
	vmemGBPerThread := int(math.Ceil(res.VMemGB / res.Threads))
	if self.memGBPerCore > vmemGBPerThread {
		vmemGBPerThread = self.memGBPerCore
	}
	memGBPerThread := vmemGBPerThread
	if self.config.alwaysVmem && res.VMemGB > res.MemGB {
		res.MemGB = res.VMemGB
	} else {
		memGBPerThread = int(math.Ceil(res.MemGB / res.Threads))
		if self.memGBPerCore > memGBPerThread {
			memGBPerThread = self.memGBPerCore
		}
	}

	mappedJobResourcesOpt := ""
	// If a __special is specified for this stage, and the runtime was called
	// with MRO_JOBRESOURCES defining a mapping from __special to a complex value
	// expression, then populate the resources option into the template. Otherwise,
	// leave it blank to revert to default behavior.
	if len(res.Special) > 0 {
		if resources, ok := self.jobResourcesMappings[res.Special]; ok {
			mappedJobResourcesOpt = strings.Replace(
				self.config.jobResourcesOpt,
				"__RESOURCES__", resources, 1)
		}
	}

	threads := int(math.Ceil(res.Threads))
	argsStr := formatArgs(threadEnvs(self, threads, envs), shellCmd, argv)
	const prefix = "__MRO_"
	const suffix = "__"
	params := [...][2]string{
		{prefix + "JOB_NAME" + suffix,
			fqname + "." + shellName},
		{prefix + "THREADS" + suffix,
			strconv.Itoa(threads)},
		{prefix + "STDOUT" + suffix,
			shellSafeQuote(metadata.MetadataFilePath("stdout"))},
		{prefix + "STDERR" + suffix,
			shellSafeQuote(metadata.MetadataFilePath("stderr"))},
		{prefix + "JOB_WORKDIR" + suffix,
			shellSafeQuote(metadata.curFilesPath)},
		{prefix + "CMD" + suffix,
			argsStr},
		{prefix + "MEM_GB" + suffix,
			strconv.Itoa(int(math.Ceil(res.MemGB)))},
		{prefix + "MEM_MB" + suffix,
			strconv.Itoa(int(math.Ceil(res.MemGB * 1024)))},
		{prefix + "MEM_KB" + suffix,
			strconv.Itoa(int(math.Ceil(res.MemGB * 1024 * 1024)))},
		{prefix + "MEM_B" + suffix,
			strconv.Itoa(int(math.Ceil(res.MemGB * 1024 * 1024 * 1024)))},
		{prefix + "MEM_GB_PER_THREAD" + suffix,
			strconv.Itoa(memGBPerThread)},
		{prefix + "MEM_MB_PER_THREAD" + suffix,
			strconv.Itoa(memGBPerThread * 1024)},
		{prefix + "MEM_KB_PER_THREAD" + suffix,
			strconv.Itoa(memGBPerThread * 1024 * 1024)},
		{prefix + "MEM_B_PER_THREAD" + suffix,
			strconv.Itoa(memGBPerThread * 1024 * 1024 * 1024)},
		{prefix + "VMEM_GB" + suffix,
			strconv.Itoa(int(math.Ceil(res.VMemGB)))},
		{prefix + "VMEM_MB" + suffix,
			strconv.Itoa(int(math.Ceil(res.VMemGB * 1024)))},
		{prefix + "VMEM_KB" + suffix,
			strconv.Itoa(int(math.Ceil(res.VMemGB * 1024 * 1024)))},
		{prefix + "VMEM_B" + suffix,
			strconv.Itoa(int(math.Ceil(res.VMemGB * 1024 * 1024 * 1024)))},
		{prefix + "VMEM_GB_PER_THREAD" + suffix,
			strconv.Itoa(vmemGBPerThread)},
		{prefix + "VMEM_MB_PER_THREAD" + suffix,
			strconv.Itoa(vmemGBPerThread * 1024)},
		{prefix + "VMEM_KB_PER_THREAD" + suffix,
			strconv.Itoa(vmemGBPerThread * 1024 * 1024)},
		{prefix + "VMEM_B_PER_THREAD" + suffix,
			strconv.Itoa(vmemGBPerThread * 1024 * 1024 * 1024)},
		{prefix + "ACCOUNT" + suffix,
			os.Getenv("MRO_ACCOUNT")},
		{prefix + "RESOURCES" + suffix,
			mappedJobResourcesOpt},
	}

	template := self.config.jobTemplate
	// Replace template annotations with actual values
	args := make([]string, 0, 2*len(params))
	for _, vals := range params {
		rkey, val := vals[0], vals[1]
		if len(val) > 0 {
			args = append(args, rkey, val)
		} else if strings.Contains(template, rkey) {
			// Remove lines containing parameter from template
			for _, line := range strings.Split(template, "\n") {
				if strings.Contains(line, rkey) {
					args = append(args, line, "")
				}
			}
		}
	}
	r := strings.NewReplacer(args...)
	return r.Replace(template)
}

// Format a shell command line to set environment variables and run the command.
//
// Handles quoting things as required.
func formatArgs(envs map[string]string, shellCmd string, argv []string) string {
	// Estimate the size of the buffer that will be required.
	argsLen := 9 + len(shellCmd)
	for _, arg := range argv {
		argsLen += 9 + len(arg)
	}
	envStrs := make([]string, 0, len(envs))
	for k, v := range envs {
		s := make([]byte, 0, len(k)+5+len(v))
		s = append(s, k...)
		s = append(s, '=')
		s = appendShellSafeQuote(s, v)
		argsLen += len(s) + 5
		envStrs = append(envStrs, string(s))
	}
	// Ensure consistent ordering.
	sort.Strings(envStrs)
	argsStr := make([]byte, 0, argsLen)
	for _, s := range envStrs {
		argsStr = append(argsStr, s...)
		argsStr = append(argsStr, " \\\n  "...)
	}
	argsStr = appendShellSafeQuote(argsStr, shellCmd)
	for _, arg := range argv {
		argsStr = append(argsStr, " \\\n  "...)
		argsStr = appendShellSafeQuote(argsStr, arg)
	}
	return string(argsStr)
}

func (self *RemoteJobManager) sendJob(shellCmd string, argv []string, envs map[string]string,
	metadata *Metadata, resRequest *JobResources, fqname string, shellName string,
	ctx context.Context) {
	jobscript := self.jobScript(shellCmd, argv, envs, metadata,
		resRequest, fqname, shellName)
	if err := metadata.WriteRaw("jobscript", jobscript); err != nil {
		util.LogError(err, "jobmngr", "Could not write job script.")
	}

	cmd := exec.CommandContext(ctx, self.config.jobCmd, self.config.jobCmdArgs...)
	cmd.Dir = metadata.curFilesPath
	cmd.Stdin = strings.NewReader(jobscript)

	// Regardless of the limiter rate, only allow one pending submission to the queue
	// at a time.  Otherwise there's a risk that if the submit command takes longer
	// than jobFreqMillis, commands will still pile up.  It's also a more "natural"
	// way to limit the submit rate if the submit server can't keep up.
	self.queueMutex.Lock()
	defer self.queueMutex.Unlock()
	if self.jobFreqMillis > 0 {
		<-(self.limiter.C)
		if self.debug {
			util.LogInfo("jobmngr", "Job rate-limit released: %s", fqname)
		}
	}

	util.EnterCriticalSection()
	defer util.ExitCriticalSection()
	if err := metadata.remove(QueuedLocally); err != nil {
		util.LogError(err, "jobmngr", "Error removing queue sentinel file.")
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		metadata.WriteErrorString(
			"jobcmd error (" + err.Error() + "):\n" + string(output))
	} else {
		trimmed := bytes.TrimSpace(output)
		// jobids should not have spaces in them.  This is the most general way to
		// check that a string is actually a jobid.
		if len(trimmed) > 0 && !bytes.ContainsAny(trimmed, " \t\n\r") {
			if err := metadata.WriteRawBytes("jobid", bytes.TrimSpace(output)); err != nil {
				util.LogError(err, "jobmngr", "Could not write job id file.")
			}
			metadata.cache("jobid", metadata.uniquifier)
		}
	}
}

func (self *RemoteJobManager) checkQueue(ids []string, ctx context.Context) ([]string, string) {
	if self.config.queueQueryCmd == "" {
		return ids, ""
	}
	jobPath := util.RelPath(path.Join("..", "jobmanagers"))
	cmd := exec.CommandContext(ctx, path.Join(jobPath, self.config.queueQueryCmd))
	cmd.Dir = jobPath
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return ids, stderr.String()
	}
	return strings.Split(string(output), "\n"), stderr.String()
}

func (self *RemoteJobManager) hasQueueCheck() bool {
	return self.config.queueQueryCmd != ""
}

func (self *RemoteJobManager) queueCheckGrace() time.Duration {
	return self.config.queueQueryGrace
}

// Reset the max jobs semaphore.
func (self *RemoteJobManager) resetMaxJobs() {
	oldSem := self.jobSem
	if oldSem != nil {
		self.jobSem = NewMaxJobsSemaphore(oldSem.Limit)
		oldSem.Clear()
	}
}

// Re-add a job to the max jobs semaphore.
func (self *RemoteJobManager) reattach(md *Metadata) {
	if self.jobSem == nil {
		return
	}
	self.jobSem.Acquire(md, true)
}
