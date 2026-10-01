#!/usr/bin/env python3
"""Recreate field.png with only Python's standard library: 320x200 RGB pixels."""
import math
from pathlib import Path
import struct
import zlib

width, height = 320, 200
rows = bytearray()
for y in range(height):
    rows.append(0)
    for x in range(width):
        r = int(255 * x / (width - 1))
        b = int(255 * y / (height - 1))
        g = int(180 * (1 - x / (width - 1)) * (1 - y / (height - 1)))
        if abs(math.hypot(x - 160, y - 100) - 65) < 2:
            r, g, b = 255, 255, 255
        if x % 40 == 0 or y % 40 == 0:
            r, g, b = 30, 40, 60
        rows.extend((r, g, b))

def chunk(kind, data):
    return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))

png = b'\x89PNG\r\n\x1a\n'
png += chunk(b'IHDR', struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0))
png += chunk(b'IDAT', zlib.compress(bytes(rows), 9)) + chunk(b'IEND', b'')
Path(__file__).with_name('field.png').write_bytes(png)
