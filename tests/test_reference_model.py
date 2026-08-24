"""Which model the instrument measures with.

PINNED IN THE REPOSITORY, not read from the operator's environment. The config
describes the DEPARTMENT's serving stack, which is a thing an operator changes;
a baseline whose model silently changes when someone edits `~/.config` is not a
baseline, because every number before the edit becomes incomparable with every
number after it and nothing says so.

Flags still win, so measuring a different model is one argument away.
"""

from blacksmith.cli import REFERENCE_ENDPOINT, REFERENCE_MODEL, resolve_model
from blacksmith.config import Config, ModelClass


def a_config(**classes) -> Config:
    return Config(host="box-a", classes=dict(classes))


def test_the_reference_model_is_used_by_default():
    model = resolve_model(a_config(), endpoint=None, name=None)
    assert model.model == REFERENCE_MODEL
    assert model._base == REFERENCE_ENDPOINT


def test_the_reference_is_qwen3_8():
    """One model, deliberately. qwen3.6 is the previous generation of the same
    thing and qwen3-coder-38 is the same weights under another tag, so measuring
    either adds a row and no information."""
    assert REFERENCE_MODEL.startswith("qwen3.8")


def test_the_operator_config_does_not_override_the_reference():
    """The department's endpoint is whatever the host is serving today. Letting
    it decide what the baseline measures makes the history untrustworthy."""
    configured = a_config(
        large=ModelClass(name="large", endpoint="http://somewhere-else/v1", model="other")
    )
    model = resolve_model(configured, endpoint=None, name=None)
    assert model.model == REFERENCE_MODEL


def test_a_flag_overrides_the_reference_model():
    model = resolve_model(a_config(), endpoint=None, name="llama3.1:8b")
    assert model.model == "llama3.1:8b"
    assert model._base == REFERENCE_ENDPOINT


def test_a_flag_overrides_the_endpoint():
    model = resolve_model(a_config(), endpoint="http://elsewhere/v1", name=None)
    assert model._base == "http://elsewhere/v1"
    assert model.model == REFERENCE_MODEL


def test_both_can_be_overridden_together():
    model = resolve_model(a_config(), endpoint="http://elsewhere/v1", name="m")
    assert (model._base, model.model) == ("http://elsewhere/v1", "m")


def test_the_reference_endpoint_is_reachable_in_principle():
    """A scheme, at least — a bare host produces relative URLs and fails a long
    way from the cause."""
    assert REFERENCE_ENDPOINT.startswith("http")
