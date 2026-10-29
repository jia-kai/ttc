"""Checks that automatic naming does not consume scripted coding replies."""
import io
import json
from pathlib import Path
import unittest

from mock_naming import respond_to_naming


class Handler:
    def __init__(self):
        self.wfile = io.BytesIO()
        self.status = None

    def send_response(self, status):
        self.status = status

    def send_header(self, *args):
        pass

    def end_headers(self):
        pass


class MockNamingTests(unittest.TestCase):
    def test_current_naming_prompt_receives_a_title(self):
        instructions = (Path(__file__).resolve().parent.parent / 'prompt' / 'naming.md').read_text()
        handler = Handler()
        self.assertTrue(respond_to_naming(handler, {'instructions': instructions}))
        self.assertEqual(handler.status, 200)
        events = [json.loads(line.removeprefix('data: ')) for line in
                  handler.wfile.getvalue().decode().splitlines() if line]
        self.assertEqual(events[0]['delta'], 'Fixture Research Session')
        self.assertEqual(events[-1]['type'], 'response.completed')

    def test_coding_request_is_not_handled(self):
        handler = Handler()
        self.assertFalse(respond_to_naming(handler, {'instructions': 'You are TTC'}))
        self.assertIsNone(handler.status)
        self.assertEqual(handler.wfile.getvalue(), b'')


if __name__ == '__main__':
    unittest.main()
