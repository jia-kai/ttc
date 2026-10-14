"""Small, dependency-free screen reader for tcell's cursor-addressed PTY frames.

This is not a general terminal emulator. It handles the cursor moves, erasure,
and printable text emitted by these full-screen fixtures; styles and terminal
modes do not change the text cells. Storage stays bounded by the terminal size.
"""
import codecs
import re
import unicodedata


class Screen:
    def __init__(self, rows, columns):
        self.rows, self.columns = rows, columns
        self.cells = [[' '] * columns for _ in range(rows)]
        self.row = self.column = 0
        self.decoder = codecs.getincrementaldecoder('utf-8')('replace')
        self.pending = ''

    @property
    def text(self):
        return '\n'.join(''.join(row).rstrip() for row in self.cells)

    def feed(self, chunk):
        text = self.pending + self.decoder.decode(chunk)
        self.pending = ''
        index = 0
        while index < len(text):
            char = text[index]
            if char == '\x1b':
                if index + 1 == len(text):
                    break
                kind = text[index + 1]
                if kind == '[':
                    match = re.match(r'\x1b\[([0-?]*)([ -/]*)([@-~])', text[index:])
                    if match is None:
                        break
                    self._csi(match[1], match[3])
                    index += match.end()
                    continue
                if kind in ']P_^':
                    end = re.search(r'\x07|\x1b\\', text[index + 2:])
                    if end is None:
                        break
                    index += 2 + end.end()
                    continue
                # Charset selection consumes a third byte; other tcell escape
                # commands do not affect the cursor-addressed screen text.
                if kind in '()':
                    if index + 2 == len(text):
                        break
                    index += 3
                else:
                    index += 2
                continue
            if char == '\r':
                self.column = 0
            elif char == '\n':
                self.row = min(self.rows - 1, self.row + 1)
            elif char == '\b':
                self.column = max(0, self.column - 1)
            elif char == '\t':
                self.column = min(self.columns - 1, (self.column // 8 + 1) * 8)
            elif ord(char) >= 32 and char != '\x7f':
                width = 0 if unicodedata.combining(char) else 2 if unicodedata.east_asian_width(char) in 'WF' else 1
                if width:
                    self.cells[self.row][self.column] = char
                    if width == 2 and self.column + 1 < self.columns:
                        self.cells[self.row][self.column + 1] = ''
                    self.column = min(self.columns - 1, self.column + width)
            index += 1
        self.pending = text[index:]

    def _csi(self, parameters, command):
        if command not in 'HfABCDGdKJ':
            return
        if parameters.startswith(('?', '>')):
            return  # Private modes, including synchronized output.
        values = [int(value or 0) for value in parameters.split(';')]
        first = values[0]
        distance = first or 1
        if command in 'Hf':
            self.row = min(self.rows - 1, max(0, distance - 1))
            self.column = min(self.columns - 1, max(0, (values[1] if len(values) > 1 else 1) - 1))
        elif command == 'A':
            self.row = max(0, self.row - distance)
        elif command == 'B':
            self.row = min(self.rows - 1, self.row + distance)
        elif command == 'C':
            self.column = min(self.columns - 1, self.column + distance)
        elif command == 'D':
            self.column = max(0, self.column - distance)
        elif command == 'G':
            self.column = min(self.columns - 1, distance - 1)
        elif command == 'd':
            self.row = min(self.rows - 1, distance - 1)
        elif command == 'K':
            start = self.column if first == 0 else 0
            end = self.column + 1 if first == 1 else self.columns
            self.cells[self.row][start:end] = [' '] * (end - start)
        elif command == 'J':
            if first in (2, 3):
                self.cells = [[' '] * self.columns for _ in range(self.rows)]
            elif first == 0:
                self.cells[self.row][self.column:] = [' '] * (self.columns - self.column)
                for row in range(self.row + 1, self.rows):
                    self.cells[row] = [' '] * self.columns
            elif first == 1:
                for row in range(self.row):
                    self.cells[row] = [' '] * self.columns
                self.cells[self.row][:self.column + 1] = [' '] * (self.column + 1)
