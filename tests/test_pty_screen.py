import unittest

from pty_screen import Screen


class ScreenTest(unittest.TestCase):
    def test_completion_reconstructed_from_cursor_patches(self):
        screen = Screen(4, 40)
        first = b'\x1b[3;1HWork complete'
        second = b'\x1b[3;1HTurn\x1b[2;1HGrowth 1 complete.'
        self.assertNotIn(b'Turn complete', first + second)
        screen.feed(first)
        screen.feed(second)
        self.assertIn('Growth 1 complete.', screen.text)
        self.assertIn('Turn complete', screen.text)

    def test_reversed_frame_order_and_fragmented_controls(self):
        screen = Screen(4, 40)
        frame = b'\x1b[?2026h\x1b[3;1H\x1b[7mTurn complete\x1b[m\x1b[2;1HGrowth 1 complete.\x1b[?2026l'
        for byte in frame:
            screen.feed(bytes([byte]))
        self.assertEqual(screen.text.splitlines()[1:3], ['Growth 1 complete.', 'Turn complete'])

    def test_erasure_replaces_previous_status(self):
        screen = Screen(4, 40)
        screen.feed(b'\x1b[3;1HTurn complete\x1b[3;1H\x1b[2KTurn failed')
        self.assertNotIn('Turn complete', screen.text)
        self.assertIn('Turn failed', screen.text)
        screen.feed(b'\x1b[2J')
        self.assertNotIn('Turn failed', screen.text)

    def test_fragmented_utf8_wide_cells_and_terminal_strings(self):
        screen = Screen(3, 20)
        frame = '\x1b]0;ignored title\x07\x1b[1;1H界\x1b[1;3Hstatus'.encode()
        for byte in frame:
            screen.feed(bytes([byte]))
        self.assertEqual(screen.text.splitlines()[0], '界status')


if __name__ == '__main__':
    unittest.main()
