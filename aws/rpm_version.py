#!/usr/bin/env python3
"""Print the VERSION field of an RPM package, for machines without the rpm tool.

An RPM file is a 96-byte lead, a signature header, padding to 8 bytes, then the main
header. Each header is: magic 8e ad e8 01, 4 reserved bytes, an entry count, a data
size, that many 16-byte entries (tag, type, offset, count), then the data. VERSION is
tag 1001, a NUL-terminated string.
"""
import struct
import sys

MAGIC = b"\x8e\xad\xe8\x01"
TAG_VERSION = 1001


def read_header(f):
    head = f.read(16)
    if len(head) < 16 or head[:4] != MAGIC:
        raise ValueError("not an RPM header")
    count, size = struct.unpack(">II", head[8:16])
    entries = [struct.unpack(">IIII", f.read(16)) for _ in range(count)]
    return entries, f.read(size), size


def version(path):
    with open(path, "rb") as f:
        f.seek(96)
        _, _, size = read_header(f)
        f.seek((8 - size % 8) % 8, 1)
        entries, data, _ = read_header(f)
    for tag, _type, offset, _count in entries:
        if tag == TAG_VERSION:
            return data[offset:data.index(b"\0", offset)].decode()
    raise ValueError("no VERSION tag")


if __name__ == "__main__":
    print(version(sys.argv[1]))
