package server

import (
	"fmt"

	"github.com/google/uuid"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/Glyph-Software/datagit/gen/datagit/v1"
	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// The wire and canonical value models must agree exactly. A value that changes
// meaning crossing this boundary would change its hash, so every conversion is
// total and explicit rather than reflective.

func valueToProto(v core.Value) *pb.Value {
	switch v.Kind {
	case core.KindNull:
		return &pb.Value{Kind: &pb.Value_IsNull{IsNull: true}}
	case core.KindBool:
		return &pb.Value{Kind: &pb.Value_BoolValue{BoolValue: v.Bool}}
	case core.KindInt:
		return &pb.Value{Kind: &pb.Value_IntValue{IntValue: v.Int}}
	case core.KindFloat:
		return &pb.Value{Kind: &pb.Value_FloatValue{FloatValue: v.Float}}
	case core.KindNumeric:
		// Carried as a normalized decimal STRING, never a float: the value is
		// hashed into history and a rounding difference would change the commit id.
		return &pb.Value{Kind: &pb.Value_NumericValue{NumericValue: v.Text}}
	case core.KindText:
		return &pb.Value{Kind: &pb.Value_TextValue{TextValue: v.Text}}
	case core.KindBytes:
		return &pb.Value{Kind: &pb.Value_BytesValue{BytesValue: v.Bytes}}
	case core.KindTime:
		return &pb.Value{Kind: &pb.Value_TimeValue{TimeValue: timestamppb.New(v.AsTime())}}
	}
	return &pb.Value{Kind: &pb.Value_IsNull{IsNull: true}}
}

func valueFromProto(v *pb.Value) (core.Value, error) {
	if v == nil {
		return core.Null(), nil
	}
	switch k := v.GetKind().(type) {
	case *pb.Value_IsNull:
		return core.Null(), nil
	case *pb.Value_BoolValue:
		return core.Bool_(k.BoolValue), nil
	case *pb.Value_IntValue:
		return core.Int(k.IntValue), nil
	case *pb.Value_FloatValue:
		return core.Float(k.FloatValue), nil
	case *pb.Value_NumericValue:
		return core.Numeric(k.NumericValue)
	case *pb.Value_TextValue:
		return core.Text(k.TextValue), nil
	case *pb.Value_BytesValue:
		return core.Bytes(k.BytesValue), nil
	case *pb.Value_TimeValue:
		return core.Time(k.TimeValue.AsTime()), nil
	}
	return core.Value{}, fmt.Errorf("unrecognized value kind")
}

func rowToProto(r core.Row) *pb.Row {
	out := &pb.Row{Cells: map[uint32]*pb.Value{}}
	for _, c := range r.Cols() {
		out.Cells[uint32(c)] = valueToProto(r[c])
	}
	return out
}

func rowFromProto(r *pb.Row) (core.Row, error) {
	if r == nil {
		return nil, nil
	}
	out := core.Row{}
	for id, v := range r.GetCells() {
		cv, err := valueFromProto(v)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", id, err)
		}
		out[core.ColID(id)] = cv
	}
	return out, nil
}

func changesFromProto(in []*pb.Change) ([]store.Change, error) {
	out := make([]store.Change, 0, len(in))
	for _, c := range in {
		row, err := rowFromProto(c.GetRow())
		if err != nil {
			return nil, err
		}
		op := core.Op(c.GetOp())
		if op == 0 {
			return nil, fmt.Errorf("change for key %x has no operation", c.GetPk())
		}
		out = append(out, store.Change{PK: core.PK(c.GetPk()), Op: op, Row: row})
	}
	return out, nil
}

// exprFromProto converts a predicate tree. There is no string form on the wire
// either, so a malicious client has no SQL to inject -- only a typed tree the
// server compiles to parameters.
func exprFromProto(e *pb.Expr) (adapter.Expr, error) {
	if e == nil {
		return nil, nil
	}
	switch n := e.GetNode().(type) {
	case *pb.Expr_Compare:
		v, err := valueFromProto(n.Compare.GetValue())
		if err != nil {
			return nil, err
		}
		op, err := compareOpFromProto(n.Compare.GetOp())
		if err != nil {
			return nil, err
		}
		return adapter.Compare{Col: core.ColID(n.Compare.GetCol()), Op: op, Value: v}, nil
	case *pb.Expr_In:
		var vals []core.Value
		for _, v := range n.In.GetValues() {
			cv, err := valueFromProto(v)
			if err != nil {
				return nil, err
			}
			vals = append(vals, cv)
		}
		return adapter.In{Col: core.ColID(n.In.GetCol()), Values: vals}, nil
	case *pb.Expr_IsNull:
		return adapter.IsNull{Col: core.ColID(n.IsNull.GetCol())}, nil
	case *pb.Expr_And:
		terms, err := exprsFromProto(n.And.GetTerms())
		return adapter.And{Terms: terms}, err
	case *pb.Expr_Or:
		terms, err := exprsFromProto(n.Or.GetTerms())
		return adapter.Or{Terms: terms}, err
	case *pb.Expr_Not:
		inner, err := exprFromProto(n.Not)
		return adapter.Not{Term: inner}, err
	}
	return nil, fmt.Errorf("unrecognized predicate node")
}

func exprsFromProto(in []*pb.Expr) ([]adapter.Expr, error) {
	out := make([]adapter.Expr, 0, len(in))
	for _, e := range in {
		c, err := exprFromProto(e)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func compareOpFromProto(o pb.CompareOp) (adapter.CompareOp, error) {
	switch o {
	case pb.CompareOp_COMPARE_OP_EQ:
		return adapter.Eq, nil
	case pb.CompareOp_COMPARE_OP_NE:
		return adapter.Ne, nil
	case pb.CompareOp_COMPARE_OP_LT:
		return adapter.Lt, nil
	case pb.CompareOp_COMPARE_OP_LE:
		return adapter.Le, nil
	case pb.CompareOp_COMPARE_OP_GT:
		return adapter.Gt, nil
	case pb.CompareOp_COMPARE_OP_GE:
		return adapter.Ge, nil
	case pb.CompareOp_COMPARE_OP_LIKE:
		return adapter.Like, nil
	}
	return 0, fmt.Errorf("unrecognized comparison operator")
}

// keyExpr builds a primary-key predicate from a canonical key, for point reads.
func keyExpr(t *store.Table, pk core.PK) (adapter.Expr, error) {
	return store.KeyExpr(t, pk)
}

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%q is not a valid session id", s)
	}
	return id, nil
}
