"""Deterministic long child answer and compact transcript assertions."""
import json
import sqlite3

CHILD_PREFIX = 'CHILD-PREVIEW 4.0'
CHILD_BODY = 'CHILD-HIDDEN-BODY'
CHILD_TAIL = 'CHILD-FULL-TAIL: all three observations independently verified.'
CHILD_ANSWER_CAP = 8 << 10  # Parent handoff cap, not the full-message inspector limit.
CHILD_ANSWER = '\n'.join([
    CHILD_PREFIX + ' ' + 'Independent arithmetic check passed for three fixture observations. ' * 6,
    '',
    '## Independent fixture evidence',
    *[f'{CHILD_BODY} {index:02d}: samples 2, 4, 6; arithmetic mean (2 + 4 + 6) / 3 = 4.0. '
      'This multiline evidence belongs in the full-message inspector, not the transcript preview.'
      for index in range(1, 57)],
    '',
    CHILD_TAIL,
])


def verify_child_answers(db, results):
    """Check capped parent answers reference complete durable child messages."""
    assert results, 'missing child results'
    expected = CHILD_ANSWER.encode()[:CHILD_ANSWER_CAP].decode()
    for result in results:
        assert result['answer'] == expected, result
        assert result['answer_truncated'] is True, result
        row = db.execute('SELECT actor_id,content_json FROM entries WHERE id=?',
                         (result['result_entry_id'],)).fetchone()
        assert row is not None and row[0] != 'main', result
        message = json.loads(row[1])
        assert message.get('type') == 'assistant' and message['content'] == CHILD_ANSWER, result
        assert 'role' not in message and 'runtime' not in message, message


def compact_rows(text, narrow=False):
    """Return one-based visible child-response and main completion row numbers.

    A screen may contain multiple completed child turns. Require one visible
    response and one returned answer; never accept leaked body or answer JSON.
    """
    assert CHILD_BODY not in text and CHILD_TAIL not in text, text
    assert '"answer"' not in text and '"answer_truncated"' not in text, text
    responses, completions = [], []
    for number, row in enumerate(text.splitlines(), 1):
        response = '[Fixture check] assistant ' in row and 'CHILD' in row
        completion = 'subagent' in row and 'done' in row and 'Fixture check' in row
        if narrow and not (response or completion):
            continue
        if not narrow and CHILD_PREFIX not in row:
            continue
        if not (response or completion) and ('Request #' in row or ' · Inspect' in row):
            continue  # Native boundary previews may contain the same answer prefix.
        assert '...' in row or '…' in row, row
        if response:
            responses.append(number)
        elif completion:
            completions.append(number)
        else:
            raise AssertionError('child prefix outside compact response/completion row: ' + row)
    assert responses and completions, text
    return responses[0], completions[0]


def child_response_target(db):
    """Return the child request's latest immutable inspection revision and title."""
    row = db.execute("""SELECT id,json_extract(content_json,'$.item.title') FROM entries
        WHERE actor_id != 'main' AND kind='status'
          AND json_extract(content_json,'$.type')='presentation_stream'
          AND json_extract(content_json,'$.direction') IN ('response','outcome')
          AND json_extract(content_json,'$.purpose')='coding'
        ORDER BY id DESC LIMIT 1""").fetchone()
    assert row is not None, 'missing child request capture'
    return row


def child_response_row(text, title):
    """Find the target boundary even when the terminal clips its semantic suffix."""
    marker = title.split(' · attempt ', 1)[0]
    matches = [number for number, row in enumerate(text.splitlines(), 1)
               if '[Fixture check]' in row and marker in row]
    assert len(matches) == 1, (marker, text)
    return matches[0]


def demo_completed(database, markdown):
    """Require a durable completed main turn and its exact notebook answer."""
    if not database.exists():
        return False
    try:
        with sqlite3.connect(f'file:{database}?mode=ro', uri=True, timeout=0.1) as db:
            turns = db.execute("SELECT status FROM turns WHERE actor_id='main'").fetchall()
            assert not any(status in ('failed', 'cancelled') for (status,) in turns), turns
            answer = db.execute("SELECT content_json FROM entries WHERE actor_id='main' AND kind='message' AND role='assistant' ORDER BY id DESC LIMIT 1").fetchone()
            return (turns == [('completed',)] and answer is not None
                    and json.loads(answer[0]).get('content') == markdown)
    except sqlite3.OperationalError:
        return False  # Database/schema creation or an in-flight writer.
