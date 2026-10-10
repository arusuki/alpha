#define _GNU_SOURCE
#include "peak.h"
#include <assert.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static void word(unsigned char *p, unsigned value)
{
    p[0] = value & 255;
    p[1] = value >> 8;
}

static void dimm(FILE *f, unsigned handle, unsigned size, unsigned configured, unsigned rated,
                 unsigned width, bool extended)
{
    unsigned char d[0x5c + 2] = {17, 0x5c};
    word(d + 2, handle);
    word(d + 8, 72); /* ECC width must not inflate the 64-bit data bus. */
    word(d + 10, width);
    word(d + 12, size);
    d[0x12] = 0x1a;
    word(d + 0x15, rated);
    word(d + 0x20, extended ? 0xffff : configured);
    if (extended)
        word(d + 0x58, configured);
    assert(fwrite(d, 1, sizeof(d), f) == sizeof(d));
}

static void channel(FILE *f, unsigned handle, unsigned first, unsigned second)
{
    unsigned char d[15] = {37, 13};
    word(d + 2, handle);
    d[4] = 3;
    d[6] = 2;
    word(d + 8, first);
    word(d + 11, second);
    assert(fwrite(d, 1, sizeof(d), f) == sizeof(d));
}

static void reset(FILE *f)
{
    assert(fflush(f) == 0);
    rewind(f);
    assert(ftruncate(fileno(f), 0) == 0);
}

static struct dbw_peak_estimate estimate(FILE *f, const char *path, unsigned channels)
{
    assert(fflush(f) == 0);
    struct dbw_peak_estimate p;
    dbw_peak_estimate_rome(path, channels, &p);
    return p;
}

int main(int argc, char **argv)
{
    if (argc == 2 && (!strcmp(argv[1], "-h") || !strcmp(argv[1], "--help"))) {
        puts("Usage: test-peak\nValidate firmware bandwidth estimation with temporary fixtures; no "
             "PMU needed.");
        return 0;
    }
    if (argc != 1)
        return 2;
    char path[] = "/tmp/dbw-peak-XXXXXX";
    int fd = mkstemp(path);
    assert(fd >= 0);
    FILE *f = fdopen(fd, "w+");
    assert(f);
    struct dbw_peak_estimate p = estimate(f, path, 16);
    assert(p.bytes_per_sec == 409.6e9 && !strcmp(p.source, "rome-spec"));
    assert(strstr(p.detail, "assuming"));
    /* Dual-socket 2-DPC: cap channels at 16, not 32 DIMMs or 128 CPU threads. */
    for (unsigned i = 0; i < 32; i++)
        dimm(f, i, 32768, 2933, 3200, 64, false);
    p = estimate(f, path, 16);
    assert(p.dimms == 32 && p.channels == 16 && p.bytes_per_sec == 375.424e9);
    assert(!strcmp(p.source, "smbios-rome") && strstr(p.detail, "balanced"));

    reset(f);
    dimm(f, 1, 0, 0, 0, 0xffff, false); /* Empty slot. */
    dimm(f, 2, 32768, 2400, 3200, 64, false);
    dimm(f, 3, 32768, 2666, 3200, 64, true);
    p = estimate(f, path, 8);
    assert(p.dimms == 2 && p.channels == 2 && p.bytes_per_sec == 38.4e9);
    /* 2-DPC must count one channel, even when not all eight are populated. */
    channel(f, 100, 2, 3);
    p = estimate(f, path, 8);
    assert(p.channels == 1 && p.bytes_per_sec == 19.2e9);
    assert(!strcmp(p.source, "smbios-channels"));
    /* Partial or duplicate maps cannot be advertised as complete channel data. */
    dimm(f, 4, 32768, 3200, 3200, 64, false);
    p = estimate(f, path, 8);
    assert(!strcmp(p.source, "smbios-rome"));
    channel(f, 101, 2, 4);
    p = estimate(f, path, 8);
    assert(!strcmp(p.source, "smbios-rome"));

    reset(f);
    /* Different channel speeds sum individually; extended configured speed. */
    dimm(f, 1, 32768, 2400, 3200, 64, true);
    dimm(f, 2, 0, 0, 0, 0, false);
    dimm(f, 3, 32768, 3200, 3200, 64, true);
    dimm(f, 4, 0, 0, 0, 0, false);
    channel(f, 100, 1, 2);
    channel(f, 101, 3, 4);
    p = estimate(f, path, 16);
    assert(p.channels == 2 && p.bytes_per_sec == 44.8e9);
    assert(strstr(p.detail, "configured speeds"));

    reset(f);
    dimm(f, 1, 32768, 0, 2666, 0xffff, false);
    p = estimate(f, path, 8);
    assert(p.bytes_per_sec == 21.328e9 && strstr(p.detail, "assumed"));
    reset(f);
    dimm(f, 1, 32768, 0, 0, 64, false);
    p = estimate(f, path, 8);
    assert(p.bytes_per_sec == 25.6e9 && strstr(p.detail, "assumed"));

    /* Malformed/truncated tables cannot leak partially decoded inventory. */
    assert(fputc(17, f) != EOF);
    p = estimate(f, path, 8);
    assert(!p.dimms && p.bytes_per_sec == 204.8e9 && !strcmp(p.source, "rome-spec"));
    reset(f);
    dimm(f, 1, 0xffff, 3200, 3200, 64, false); /* Unknown capacity. */
    p = estimate(f, path, 8);
    assert(!p.dimms && !strcmp(p.source, "rome-spec"));
    reset(f);
    dimm(f, 1, 32768, 3200, 3200, 64, false);
    dimm(f, 1, 32768, 3200, 3200, 64, false); /* Duplicate handle. */
    p = estimate(f, path, 8);
    assert(!p.dimms && !strcmp(p.source, "rome-spec"));
    reset(f);
    assert(fputs("\21\3\1\1\0\0", f) >= 0); /* Invalid formatted length. */
    p = estimate(f, path, 8);
    assert(!strcmp(p.source, "rome-spec"));
    fclose(f);
    unlink(path);
    dbw_peak_estimate_rome(path, 16, &p); /* Missing/permission-denied firmware. */
    assert(p.bytes_per_sec == 409.6e9 && !strcmp(p.source, "rome-spec"));
    dbw_peak_estimate_rome(path, 0, &p);
    assert(p.bytes_per_sec == 0);
    puts("peak: configured speeds, ECC, population, shared channels, fallback and malformed "
         "firmware passed");
    return 0;
}
