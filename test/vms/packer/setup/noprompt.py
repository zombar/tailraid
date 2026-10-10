"""noprompt.py <windows.iso> <mount point> <out.iso>: a copy of the Windows ISO whose EFI
boot image no longer asks to "press any key to boot from CD".

The ISO's El Torito EFI boot image is efi/microsoft/boot/efisys.bin, a FAT image whose
loader prints the prompt and, unanswered, hands control back to the firmware. Microsoft
ships efisys_noprompt.bin beside it, the same image without the prompt and of the same
size. ISO 9660 stores a file as one contiguous extent, so the copy is patched in place:
the extent of efisys.bin is found by its own bytes and overwritten with the no-prompt
image. Nothing else changes, so every other file and the boot catalog stay as they were.
"""
import mmap
import os
import shutil
import sys

iso, mnt, out = sys.argv[1], sys.argv[2], sys.argv[3]
boot = os.path.join(mnt, "efi", "microsoft", "boot")
with open(os.path.join(boot, "efisys.bin"), "rb") as f:
    prompt = f.read()
with open(os.path.join(boot, "efisys_noprompt.bin"), "rb") as f:
    noprompt = f.read()
if len(prompt) != len(noprompt):
    sys.exit("efisys.bin is %d bytes and efisys_noprompt.bin %d: cannot patch in place" % (len(prompt), len(noprompt)))
if not os.path.exists(out) or os.path.getsize(out) != os.path.getsize(iso):
    print("copying %s (%.1f GB)" % (iso, os.path.getsize(iso) / 1e9))
    shutil.copyfile(iso, out)
# the two images share their FAT header, so the needle is a window around the first
# byte that differs (the loader itself)
d = next(i for i in range(len(prompt)) if prompt[i] != noprompt[i])
start = max(0, d - 32768)
needle = prompt[start:start + 65536]
# the image occurs twice: as the file and as the El Torito boot image the catalog points
# at (the one the firmware loads); both are replaced
patched = []
with open(out, "r+b") as f:
    m = mmap.mmap(f.fileno(), 0)
    hit = m.find(needle)
    while hit >= 0:
        off = hit - start
        if m[off:off + len(prompt)] != prompt:
            sys.exit("efisys.bin is not contiguous at offset %d" % off)
        m[off:off + len(noprompt)] = noprompt
        patched.append(off)
        hit = m.find(needle, hit + 1)
    if not patched:
        if m.find(noprompt[start:start + 65536]) >= 0:
            print("already patched")
            sys.exit(0)
        sys.exit("efisys.bin's bytes not found in the ISO")
    m.flush()
    m.close()
print("patched efisys.bin (%d bytes) at offsets %s in %s" % (len(noprompt), patched, out))
