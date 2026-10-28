"""Checks for the shared Go/Python retry-budget definition."""
from pathlib import Path
import tempfile
import unittest

from retry_policy import default_max_attempts


class RetryPolicyTests(unittest.TestCase):
    def test_reads_go_definition(self):
        self.assertGreater(default_max_attempts(), 0)

    def test_integer_constant(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / 'llm.go'
            source.write_text('package llm\n\nconst DefaultMaxAttempts = 17\n')
            self.assertEqual(default_max_attempts(source), 17)

    def test_rejects_missing_ambiguous_or_unsupported_definition(self):
        for definition in ('', 'const DefaultMaxAttempts = 0\n',
                           'const DefaultMaxAttempts = -1\n',
                           'const DefaultMaxAttempts = 1 + 2\n',
                           'const DefaultMaxAttempts = 2\n' * 2):
            with self.subTest(definition=definition), tempfile.TemporaryDirectory() as temporary:
                source = Path(temporary) / 'llm.go'
                source.write_text(definition)
                with self.assertRaises(ValueError):
                    default_max_attempts(source)


if __name__ == '__main__':
    unittest.main()
