package iis

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/stats"
	shelpers "github.com/hashicorp/nomad/helper/stats"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// taskHandle should store all relevant runtime information
// such as process ID if this is a local task or other meta
// data if this driver deals with external APIs
type taskHandle struct {
	// stateLock syncs access to all fields below
	stateLock sync.RWMutex

	logger         hclog.Logger
	taskConfig     *drivers.TaskConfig
	procState      drivers.TaskState
	startedAt      time.Time
	completedAt    time.Time
	exitResult     *drivers.ExitResult
	websiteStopped chan interface{}
	totalCpuStats  *stats.CpuStats
	userCpuStats   *stats.CpuStats
	systemCpuStats *stats.CpuStats
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) run(driverConfig *TaskConfig) {
	// Every executor runs this init at creation for stats
	if err := shelpers.Init(); err != nil {
		h.logger.Error("unable to initialize stats", "error", err)
	}

	if !filepath.IsAbs(driverConfig.Path) {
		driverConfig.Path = filepath.Join(h.taskConfig.TaskDir().Dir, driverConfig.Path)
	}

	// Gather Network Ports: http or https only
	networks := h.taskConfig.Resources.NomadResources.Networks
	if len(networks) == 0 {
		errMsg := "Error in launching task: Trying to map ports but no network interface is available"
		h.handleError(errMsg, errors.New(errMsg))
		return
	}

	var iisBindings []iisBinding
	for _, binding := range driverConfig.Bindings {
		if binding.Port == 0 && binding.ResourcePort == "" {
			errMsg := "Error in launching task: both binding.Port and binding.ResourcePort cannot be unset."
			h.handleError(errMsg, errors.New(errMsg))
			return
		}

		if binding.Port < 0 {
			errMsg := "Error in launching task: binding.Port cannot be negative."
			h.handleError(errMsg, errors.New(errMsg))
			return
		}

		if binding.Port > 0 {
			iisBindings = append(iisBindings, binding)
			continue
		}

		for _, network := range networks {
			for _, dynamicPort := range network.DynamicPorts {
				if binding.ResourcePort == dynamicPort.Label {
					binding.Port = dynamicPort.Value
					iisBindings = append(iisBindings, binding)
				}
			}
			for _, staticPort := range network.ReservedPorts {
				if binding.ResourcePort == staticPort.Label {
					binding.Port = staticPort.Value
					iisBindings = append(iisBindings, binding)
				}
			}
		}
	}

	driverConfig.Bindings = iisBindings

	if err := createWebsite(h.taskConfig.AllocID, driverConfig); err != nil {
		errMsg := fmt.Sprintf("Error in creating website: %v", err)
		h.handleError(errMsg, err)
		return
	}

	if err := startWebsite(h.taskConfig.AllocID); err != nil {
		errMsg := fmt.Sprintf("Error in starting website: %v", err)
		h.handleError(errMsg, err)
		return
	}

	go h.wait()
}

// handleError will log the error message (errMsg) and update the task handle with exit results.
func (h *taskHandle) handleError(errMsg string, err error) {
	h.logger.Error(errMsg)
	h.exitResult.Err = err
	h.procState = drivers.TaskStateUnknown
	h.completedAt = time.Now()
}

func (h *taskHandle) Stats(ctx context.Context, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	ch := make(chan *drivers.TaskResourceUsage)
	go h.handleStats(ch, ctx, interval)

	return ch, nil
}

func (h *taskHandle) handleStats(ch chan *drivers.TaskResourceUsage, ctx context.Context, interval time.Duration) {
	defer close(ch)
	timer := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			timer.Reset(interval)
		}

		t := time.Now()

		// Get IIS Worker Process stats if we can.
		stats, err := getWebsiteStats(h.taskConfig.AllocID)
		if err != nil {
			//h.logger.Error("Failed to get iis worker process stats:", "error", err)
			return
		}
		var cs drivers.CpuStats
		var ms drivers.MemoryStats

		total := stats.KernelModeTime + stats.UserModeTime
		cs.SystemMode = h.systemCpuStats.Percent(float64(stats.KernelModeTime))
		cs.UserMode = h.userCpuStats.Percent(float64(stats.UserModeTime))
		cs.Percent = h.totalCpuStats.Percent(float64(total))
		cs.TotalTicks = h.totalCpuStats.TicksConsumed(cs.Percent)
		cs.Measured = []string{"Percent", "System Mode", "User Mode"}

		ms.RSS = stats.WorkingSetPrivate
		ms.Measured = []string{"RSS"}

		taskResUsage := drivers.TaskResourceUsage{
			ResourceUsage: &drivers.ResourceUsage{
				CpuStats:    &cs,
				MemoryStats: &ms,
			},
			Timestamp: t.UTC().UnixNano(),
		}

		select {
		case <-ctx.Done():
			return
		case ch <- &taskResUsage:
		}
	}
}

func (h *taskHandle) wait() {
	defer close(h.websiteStopped)
	for {
		isRunning, err := isWebsiteRunning(h.taskConfig.AllocID)
		if err != nil {
			h.logger.Error("Error in getting task status: %v", err)
			h.procState = drivers.TaskStateExited
			return
		}
		if !isRunning {
			h.procState = drivers.TaskStateExited
			return
		}
	}
}

func (h *taskHandle) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.websiteStopped:
		return nil
	}
}

func (h *taskHandle) shutdown(timeout time.Duration) error {
	// Ensure IIS is up to date with timeout config before turning off
	err := applyWebsiteShutdownTimeout(h.taskConfig.AllocID, timeout)
	if err != nil {
		return err
	}

	// Stops future traffic for website
	// Existing connections will remain until timeout is hit
	err = stopWebsite(h.taskConfig.AllocID)
	if err != nil {
		return err
	}

	return nil
}

func (h *taskHandle) cleanup() error {
	err := deleteWebsite(h.taskConfig.AllocID)
	if err != nil {
		return fmt.Errorf("Error in destroying website: %v", err)
	}

	return nil
}
