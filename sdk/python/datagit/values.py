"""Conversion between Python values and DataGit's canonical wire values.

The mapping is EXPLICIT and total. There is no "figure out what this is" path,
because the one ambiguous case -- a number -- is the case that matters most:
sending a Python float where the column holds an exact decimal is how a value
silently changes on its way into a hash.
"""

from __future__ import annotations

import datetime as _dt
from decimal import Decimal
from typing import Any

from google.protobuf.timestamp_pb2 import Timestamp

from .v1 import datagit_pb2 as pb


def to_wire(v: Any) -> pb.Value:
    """Convert a Python value to a wire Value.

    `Decimal` becomes an exact numeric carried as a string. `float` becomes a
    float and is REFUSED for anything that needs exactness -- if the column is a
    decimal, pass a Decimal.
    """
    if v is None:
        return pb.Value(is_null=True)
    if isinstance(v, bool):  # before int: bool is an int in Python
        return pb.Value(bool_value=v)
    if isinstance(v, int):
        return pb.Value(int_value=v)
    if isinstance(v, Decimal):
        return pb.Value(numeric_value=str(v))
    if isinstance(v, float):
        return pb.Value(float_value=v)
    if isinstance(v, str):
        return pb.Value(text_value=v)
    if isinstance(v, (bytes, bytearray)):
        return pb.Value(bytes_value=bytes(v))
    if isinstance(v, _dt.datetime):
        ts = Timestamp()
        ts.FromDatetime(v)
        return pb.Value(time_value=ts)
    raise TypeError(
        f"cannot send {type(v).__name__} to DataGit. Use Decimal for exact "
        f"numbers, datetime for timestamps, or bytes for binary"
    )


def from_wire(v: pb.Value) -> Any:
    """Convert a wire Value to a Python value."""
    which = v.WhichOneof("kind")
    if which is None or which == "is_null":
        return None
    if which == "bool_value":
        return v.bool_value
    if which == "int_value":
        return v.int_value
    if which == "float_value":
        return v.float_value
    if which == "numeric_value":
        # A Decimal, not a float. Round-tripping through float would lose the
        # exactness the wire format went to trouble to preserve.
        return Decimal(v.numeric_value)
    if which == "text_value":
        return v.text_value
    if which == "bytes_value":
        return v.bytes_value
    if which == "time_value":
        return v.time_value.ToDatetime()
    raise ValueError(f"unknown value kind {which}")
