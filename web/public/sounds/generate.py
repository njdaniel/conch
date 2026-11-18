#!/usr/bin/env python3
"""Generate Conch's project-made, deterministic 16-bit mono PCM cues."""
import math
from pathlib import Path
import struct
import wave

RATE = 24000
NOTES = {'message': (660, 880), 'join': (440, 660),
         'leave': (660, 440), 'share': (523, 659, 784)}
for name, notes in NOTES.items():
    samples = []
    length = int(RATE * 0.09)
    for frequency in notes:
        for i in range(length):
            envelope = math.sin(math.pi * i / (length - 1)) ** 2
            samples.append(round(8000 * envelope * math.sin(2 * math.pi * frequency * i / RATE)))
    with wave.open(str(Path(__file__).with_name(name + '.wav')), 'wb') as output:
        output.setparams((1, 2, RATE, 0, 'NONE', 'not compressed'))
        output.writeframes(struct.pack('<' + 'h' * len(samples), *samples))
