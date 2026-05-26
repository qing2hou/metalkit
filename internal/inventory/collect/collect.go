package collect

import (
	"context"
	"time"

	"metalkit/internal/inventory"
)

// All runs every collector with a per-collector timeout, accumulates soft
// failures into Report.Agent.Errors, and never returns a hard error unless the
// caller's context is cancelled.
func All(ctx context.Context, agentVersion string) (*inventory.Report, error) {
	started := time.Now()

	r := &inventory.Report{
		SchemaVersion: inventory.SchemaVersion,
		AgentVersion:  agentVersion,
		CollectedAt:   started.UTC(),
	}

	collectors := []struct {
		name string
		run  func(ctx context.Context, r *inventory.Report) error
	}{
		// Order matters slightly: cheap /proc reads first so we always have
		// something even if heavy tools hang. dmidecode populates DIMMs which
		// memory.go relies on existing — but the two only share Memory.DIMMs
		// (dmidecode writes, memory reads nothing from it), so order is loose.
		{"system", collectSystem},
		{"cpu", collectCPU},
		{"memory", collectMemory},
		{"dmidecode", collectDMIDecode},
		{"disks", collectDisks},
		{"nics", collectNICs},
		{"pci", collectPCI},
		{"bmc", collectBMC},
		{"sensors", collectSensors},
	}

	for _, c := range collectors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.run(cctx, r)
		cancel()
		if err != nil {
			r.Agent.Errors = append(r.Agent.Errors, inventory.CollectError{
				Collector: c.name,
				Err:       err.Error(),
			})
		}
	}

	dur := time.Since(started).Milliseconds()
	r.CollectionDurationMS = dur
	r.Agent.Version = agentVersion
	r.Agent.CollectedAt = r.CollectedAt
	r.Agent.CollectionDurationMS = dur
	return r, nil
}
