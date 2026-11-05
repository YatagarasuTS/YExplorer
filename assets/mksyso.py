# Генерирует rsrc_windows_amd64.syso с иконкой приложения (RT_ICON + RT_GROUP_ICON)
import struct, sys
from PIL import Image
import io
src, out = sys.argv[1], sys.argv[2]
im = Image.open(src).convert('RGBA')
sizes = [16, 24, 32, 48, 64, 128, 256]
imgs = []
for s in sizes:
    b = io.BytesIO(); im.resize((s, s), Image.LANCZOS).save(b, 'PNG'); imgs.append((s, b.getvalue()))
n = len(imgs)
# group icon data
grp = struct.pack('<HHH', 0, 1, n)
for i, (s, d) in enumerate(imgs):
    grp += struct.pack('<BBBBHHIH', s % 256, s % 256, 0, 0, 1, 32, len(d), i + 1)
# layout of directory tree
def dir_hdr(nid): return struct.pack('<IIHHHH', 0, 0, 0, 0, 0, nid)
offs = {}
pos = 0
root_off = pos; pos += 16 + 8 * 2
t3_off = pos; pos += 16 + 8 * n
t14_off = pos; pos += 16 + 8
lang_icon = []
for i in range(n): lang_icon.append(pos); pos += 16 + 8
lang_grp = pos; pos += 16 + 8
de_icon = []
for i in range(n): de_icon.append(pos); pos += 16
de_grp = pos; pos += 16
datas = [d for _, d in imgs] + [grp]
data_off = []
for d in datas:
    pos = (pos + 7) & ~7
    data_off.append(pos); pos += len(d)
buf = bytearray(pos)
def put(o, b): buf[o:o + len(b)] = b
put(root_off, dir_hdr(2) + struct.pack('<II', 3, 0x80000000 | t3_off) + struct.pack('<II', 14, 0x80000000 | t14_off))
put(t3_off, dir_hdr(n) + b''.join(struct.pack('<II', i + 1, 0x80000000 | lang_icon[i]) for i in range(n)))
put(t14_off, dir_hdr(1) + struct.pack('<II', 1, 0x80000000 | lang_grp))
for i in range(n): put(lang_icon[i], dir_hdr(1) + struct.pack('<II', 0x409, de_icon[i]))
put(lang_grp, dir_hdr(1) + struct.pack('<II', 0x409, de_grp))
relocs = []
for i in range(n):
    put(de_icon[i], struct.pack('<IIII', data_off[i], len(datas[i]), 0, 0)); relocs.append(de_icon[i])
put(de_grp, struct.pack('<IIII', data_off[n], len(datas[n]), 0, 0)); relocs.append(de_grp)
for o, d in zip(data_off, datas): put(o, d)
# COFF
hdr_size = 20 + 40
raw_ptr = hdr_size
reloc_ptr = raw_ptr + len(buf)
sym_ptr = reloc_ptr + 10 * len(relocs)
f = struct.pack('<HHIIIHH', 0x8664, 1, 0, sym_ptr, 1, 0, 0)
sh = struct.pack('<8sIIIIIIHHI', b'.rsrc\0\0\0', 0, 0, len(buf), raw_ptr, reloc_ptr, 0, len(relocs), 0, 0x40000040)
rel = b''.join(struct.pack('<IIH', o, 0, 3) for o in relocs)
sym = struct.pack('<8sIhHBB', b'.rsrc\0\0\0', 0, 1, 0, 3, 0)
open(out, 'wb').write(f + sh + bytes(buf) + rel + sym + struct.pack('<I', 4))
print('ok', out, len(buf))
