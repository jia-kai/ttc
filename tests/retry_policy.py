"""Read the upstream retry budget from its authoritative Go definition."""
from pathlib import Path
import re


def default_max_attempts(source=None):
    """Require one positive integer constant; fail if the Go definition changes shape."""
    if source is None:
        source = Path(__file__).resolve().parents[1] / 'internal/llm/llm.go'
    matches = re.findall(r'^const DefaultMaxAttempts = ([0-9]+)\s*$',
                         source.read_text(), re.MULTILINE)
    if len(matches) != 1 or int(matches[0]) < 1:
        raise ValueError(f'Expected one positive integer DefaultMaxAttempts in {source}')
    return int(matches[0])
