package mysql

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
)

// Partitioning by seq_from range (§14.3).
//
// One difference from PostgreSQL is genuine and worth stating rather than
// hiding: MySQL RANGE partitioning takes an INTEGER expression, so it cannot
// partition on (branch_id, seq_from) the way PostgreSQL does. MySQL partitions
// on seq_from alone.
//
// The consequence is that a partition spans every branch for its sequence range,
// so dropping one drops that range for all branches at once. Retention already
// works that way — a prune is by age or depth across the repository, not per
// branch — so pruning by partition drop still holds. What MySQL cannot do is
// drop one branch's history by dropping a partition; that falls back to a
// DELETE, which is what deleting a branch already did.

// PartitionSpec describes one range partition. BranchID is accepted and ignored
// on MySQL, so callers can use one spec type against both engines.
type PartitionSpec struct {
	BranchID       [16]byte
	FromSeq, ToSeq int64
}

// CreatePartitionedSidecar creates a sidecar partitioned by seq_from.
//
// MySQL requires every partitioning column to be part of every unique key, which
// holds here: the primary key is (branch_id, pk..., seq_from).
func (a *Adapter) CreatePartitionedSidecar(ctx context.Context, tx adapter.Tx,
	t *adapter.TableSpec) error {

	// MAXVALUE catches everything past the last declared bound, so a write can
	// never fail for want of a partition. Correctness first: a missing partition
	// would turn a storage optimization into a write outage.
	return a.createSidecarDDL(ctx, tx, t,
		"\nPARTITION BY RANGE (seq_from) (\n"+
			"  PARTITION p_max VALUES LESS THAN MAXVALUE\n)")
}

// AddPartition splits the catch-all partition at a sequence bound.
//
// REORGANIZE rather than ADD, because MySQL can only ADD past the highest bound
// and the catch-all is always highest. Reorganizing rewrites only the partition
// being split.
func (a *Adapter) AddPartition(ctx context.Context, tx adapter.Tx, t *adapter.TableSpec,
	spec PartitionSpec) error {

	name := partitionName(spec)
	return tx.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s REORGANIZE PARTITION p_max INTO (
		   PARTITION %s VALUES LESS THAN (%d),
		   PARTITION p_max VALUES LESS THAN MAXVALUE)`,
		quoteIdent(catalog.SidecarTable(t.PhysicalName)), quoteIdent(name), spec.ToSeq))
}

// DropPartition removes a partition and everything in it.
func (a *Adapter) DropPartition(ctx context.Context, tx adapter.Tx, physical string,
	spec PartitionSpec) error {

	return tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DROP PARTITION %s`,
		quoteIdent(catalog.SidecarTable(physical)), quoteIdent(partitionName(spec))))
}

func partitionName(spec PartitionSpec) string {
	return fmt.Sprintf("p_%d", spec.FromSeq)
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
