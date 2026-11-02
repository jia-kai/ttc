"""Assertions/readers for the tagged YAML emitted to local provider fixtures.

These fixtures read only top-level scalar fields, not arbitrary YAML documents.
Full YAML values and escaping are independently tested at the Go HTTP boundary;
keeping these readers narrow avoids adding a Python YAML package prerequisite.
"""
import json
import re


def model_data(text, name):
    prefix, suffix = f'<{name}>\n', f'\n</{name}>'
    assert text.startswith(prefix) and text.endswith(suffix), (name, text)
    return text[len(prefix):-len(suffix)]


def scalar_field(data, name):
    """Read a fixture's top-level plain/JSON-quoted/literal scalar only."""
    lines = data.split('\n')
    matches = [(index, line[len(name) + 1:].strip())
               for index, line in enumerate(lines) if line.startswith(name + ':')]
    assert len(matches) == 1, (name, data)
    index, value = matches[0]
    if value in ('|', '|-', '|+'):
        block = []
        for line in lines[index + 1:]:
            if line and not line.startswith(' '):
                break
            block.append(line)
        nonempty = [line for line in block if line]
        indent = min(len(line) - len(line.lstrip(' ')) for line in nonempty) if nonempty else 0
        text = '\n'.join(line[indent:] for line in block)
        if value == '|-':
            return text.rstrip('\n')
        if value == '|':
            return text.rstrip('\n') + '\n'
        return text
    if value.startswith('"') or value in ('true', 'false', 'null') or re.fullmatch(r'-?\d+', value):
        return json.loads(value)
    if value.startswith("'") and value.endswith("'"):
        return value[1:-1].replace("''", "'")
    assert value and not value.startswith(('[', '{', '>', '|')), (name, value)
    return value


def runtime_context(body):
    """Return actor/click fields used by mocks; verify baseline/update markers."""
    contexts = [item['content'][0]['text'] for item in body['input']
                if item.get('role') == 'developer'
                and item['content'][0].get('text', '').startswith('<runtime_context')]
    assert contexts, 'missing initial runtime context'
    baseline = None
    for text in contexts:
        name = 'runtime_context_update' if text.startswith('<runtime_context_update>') else 'runtime_context'
        data = model_data(text, name)
        assert not re.search(r'^(type|set):', data, re.MULTILINE), data
        if name == 'runtime_context':
            baseline = data
    assert baseline is not None, contexts
    result = {'actor': scalar_field(baseline, 'actor')}
    if re.search(r'^image_click:', baseline, re.MULTILINE):
        result['image_click'] = scalar_field(baseline, 'image_click')
    return result
