package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
)

// Partitioning by (branch_id, seq_from) range (§14.3).
//
// The payoff is pruning: dropping a partition is a catalogue operation and a
// file unlink, where deleting the same rows is a full scan plus index
// maintenance plus vacuum. Phase 0 measured pruning by partition drop at 33.7×
// the speed of the equivalent DELETE.
//
// It is NOT the default. A partitioned sidecar must be created partitioned --
// PostgreSQL cannot convert a populated table in place -- so turning it on for
// an existing table means rewriting it. Offering it as an opt-in for tables
// whose history actually grows is honest; making it universal would impose that
// rewrite on tables that will never need it.

// PartitionSpec describes one range partition.
type PartitionSpec struct {
	BranchID [16]byte
	// FromSeq and ToSeq are the half-open sequence range, matching the interval
	// convention the sidecar itself uses (§5.2d).
	FromSeq, ToSeq int64
}

// CreatePartitionedSidecar creates a sidecar declared PARTITION BY RANGE.
//
// PostgreSQL requires every partitioning column to be part of the primary key,
// which is already true here: the key is (branch_id, pk..., seq_from).
func (a *Adapter) CreatePartitionedSidecar(ctx context.Context, tx adapter.Tx,
	t *adapter.TableSpec) error {

	if err := a.createSidecarDDL(ctx, tx, t, " PARTITION BY RANGE (branch_id, seq_from)"); err != nil {
		return err
	}
	// A DEFAULT partition catches anything no explicit range covers, so a write
	// never fails for want of a partition. Without it, a branch created after the
	// last AddPartition call would be unable to write at all -- correctness
	// sacrificed for a storage optimization, which is the wrong trade.
	return tx.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s DEFAULT`,
		quoteIdent(catalog.SidecarTable(t.PhysicalName)+"_default"),
		quoteIdent(catalog.SidecarTable(t.PhysicalName))))
}

// AddPartition adds one range partition.
//
// The bounds are interpolated rather than bound as parameters: PostgreSQL does
// not accept parameters in DDL, and a partition bound is part of the table
// definition. Both values are safe to interpolate by construction -- a UUID
// rendered in its canonical form, and an int64 -- so there is no user text here
// to escape.
func (a *Adapter) AddPartition(ctx context.Context, tx adapter.Tx, t *adapter.TableSpec,
	spec PartitionSpec) error {

	name := partitionName(t.PhysicalName, spec)
	branch := uuid.UUID(spec.BranchID).String()
	err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s
		 FOR VALUES FROM ('%s', %d) TO ('%s', %d)`,
		quoteIdent(name), quoteIdent(catalog.SidecarTable(t.PhysicalName)),
		branch, spec.FromSeq, branch, spec.ToSeq))
	if err != nil && strings.Contains(err.Error(), "default partition") {
		// PostgreSQL refuses to carve a range out from under the default partition
		// once rows in that range are already sitting there. Saying so is more use
		// than passing the constraint violation through: the fix is to declare
		// partitions for sequence ranges BEFORE writing into them.
		return fmt.Errorf(
			"cannot add a partition for seq [%d, %d): versions in that range are "+
				"already in the default partition, and PostgreSQL will not move them. "+
				"Declare partitions ahead of the sequences they will hold (§14.3): %w",
			spec.FromSeq, spec.ToSeq, err)
	}
	return err
}

// DropPartition removes a partition and everything in it.
//
// This is the operation partitioning exists for: it is a catalogue change and a
// file unlink, not a scan.
func (a *Adapter) DropPartition(ctx context.Context, tx adapter.Tx, physical string,
	spec PartitionSpec) error {

	return tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`,
		quoteIdent(partitionName(physical, spec))))
}

func partitionName(physical string, spec PartitionSpec) string {
	return fmt.Sprintf("%s_p%x_%d", catalog.SidecarTable(physical), spec.BranchID[:4], spec.FromSeq)
}

// Partitioner exposes partitioning through the engine-neutral interface.
func (a *Adapter) Partitioner() adapter.Partitioner { return partitioner{a} }

type partitioner struct{ a *Adapter }

func (p partitioner) CreatePartitioned(ctx context.Context, tx adapter.Tx, t *adapter.TableSpec) error {
	return p.a.CreatePartitionedSidecar(ctx, tx, t)
}

func (p partitioner) Add(ctx context.Context, tx adapter.Tx, t *adapter.TableSpec, part adapter.Partition) error {
	return p.a.AddPartition(ctx, tx, t, PartitionSpec(part))
}

func (p partitioner) Drop(ctx context.Context, tx adapter.Tx, physical string, part adapter.Partition) error {
	return p.a.DropPartition(ctx, tx, physical, PartitionSpec(part))
}
