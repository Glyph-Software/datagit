"""Value conversion is where an SDK silently corrupts data, so it is tested
first and hardest.
"""

import datetime as dt
from decimal import Decimal

import pytest

from datagit.values import from_wire, to_wire


def test_decimal_round_trips_exactly():
    # The reason this matters: the value is hashed into history. A rounding
    # difference on the way through the SDK would change the commit id for a
    # value the user never changed.
    for s in ["0.10", "1234567890.123456789", "-0.00", "1e10", "0"]:
        got = from_wire(to_wire(Decimal(s)))
        assert isinstance(got, Decimal), f"{s} came back as {type(got).__name__}"
        assert got == Decimal(s), f"{s} round-tripped to {got}"


def test_decimal_does_not_become_a_float():
    wire = to_wire(Decimal("0.1"))
    assert wire.WhichOneof("kind") == "numeric_value"
    assert wire.numeric_value == "0.1"
    # The classic failure: 0.1 is not representable as a float, so a float
    # round-trip would not compare equal here.
    assert from_wire(wire) == Decimal("0.1")


def test_bool_is_not_an_int():
    # bool is a subclass of int in Python, so a naive isinstance order sends
    # True as the integer 1 and the column type quietly disagrees.
    assert to_wire(True).WhichOneof("kind") == "bool_value"
    assert to_wire(1).WhichOneof("kind") == "int_value"
    assert from_wire(to_wire(True)) is True


def test_none_is_null_not_empty():
    wire = to_wire(None)
    assert wire.WhichOneof("kind") == "is_null"
    assert from_wire(wire) is None
    # An empty string is a value, not a null.
    assert from_wire(to_wire("")) == ""


def test_bytes_and_text_stay_distinct():
    assert from_wire(to_wire(b"\x00\xff")) == b"\x00\xff"
    assert from_wire(to_wire("hello")) == "hello"


def test_datetime_round_trips():
    now = dt.datetime(2026, 9, 2, 12, 30, 45, tzinfo=dt.timezone.utc)
    got = from_wire(to_wire(now))
    assert got.replace(tzinfo=dt.timezone.utc) == now


def test_unsupported_type_is_refused_with_advice():
    with pytest.raises(TypeError) as e:
        to_wire({"not": "a value"})
    assert "Decimal" in str(e.value), "the refusal should say what to use instead"
