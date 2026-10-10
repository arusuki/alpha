#define _GNU_SOURCE
#include "peak.h"
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Bounded reads also work on sysfs files whose stat size is zero. */
#define MAX_DMI_SIZE (4u * 1024u * 1024u)
#define MAX_DIMMS 1024
#define MAX_REFS (2 * MAX_DIMMS)
#define ROME_RATE 3200u

struct dimm {
    unsigned handle, rate;
    bool populated, assumed;
    int channel;
};
struct channel_ref {
    unsigned handle, channel;
};
struct memory {
    struct dimm dimms[MAX_DIMMS];
    struct channel_ref refs[MAX_REFS];
    unsigned count, ref_count, channel_count;
};

static unsigned le16(const unsigned char *p)
{
    return p[0] | (unsigned)p[1] << 8;
}

static uint32_t le32(const unsigned char *p)
{
    return le16(p) | (uint32_t)le16(p + 2) << 16;
}

static unsigned speed(const unsigned char *d, size_t len, size_t offset, size_t extended)
{
    if (len < offset + 2)
        return 0;
    unsigned value = le16(d + offset);
    if (value == 0xffff)
        value = len >= extended + 4 ? le32(d + extended) : 0;
    /* Rome cannot run above DDR4-3200; reject firmware garbage. */
    return value >= 400 && value <= ROME_RATE ? value : 0;
}

static bool decode(const unsigned char *data, size_t size, struct memory *m)
{
    size_t pos = 0;
    while (pos < size) {
        if (size - pos < 4)
            return false;
        const unsigned char *d = data + pos;
        size_t len = d[1], end;
        if (len < 4 || len > size - pos)
            return false;
        for (end = pos + len; end + 1 < size && (data[end] || data[end + 1]); end++)
            ;
        if (end + 1 >= size)
            return false;
        if (d[0] == 127)
            return true;
        if (d[0] == 17) {
            if (len < 0x15 || m->count == MAX_DIMMS)
                return false;
            unsigned handle = le16(d + 2);
            for (unsigned i = 0; i < m->count; i++)
                if (m->dimms[i].handle == handle)
                    return false;
            struct dimm *v = &m->dimms[m->count++];
            *v = (struct dimm){.handle = handle, .channel = -1};
            unsigned capacity = le16(d + 0x0c);
            if (!capacity)
                goto next; /* Empty slots do not add bandwidth. */
            if (capacity == 0xffff || (capacity == 0x7fff && (len < 0x20 || !le32(d + 0x1c))))
                return false;    /* Unknown population must not look like an empty slot. */
            if (d[0x12] != 0x1a) /* Rome uses DDR4, not persistent/other memory. */
                return false;
            unsigned width = le16(d + 0x0a); /* Data Width excludes ECC bits. */
            if (width != 64 && width != 0 && width != 0xffff)
                return false;
            v->populated = true;
            v->rate = speed(d, len, 0x20, 0x58); /* Configured, not rated speed. */
            v->assumed = !v->rate || width != 64;
            if (!v->rate)
                v->rate = speed(d, len, 0x15, 0x54);
            if (!v->rate)
                v->rate = ROME_RATE;
        } else if (d[0] == 37) {
            if (len < 7 || len < 7u + 3u * d[6] || m->channel_count == MAX_DIMMS)
                return false;
            for (unsigned i = 0; i < d[6]; i++) {
                if (m->ref_count == MAX_REFS)
                    return false;
                m->refs[m->ref_count++] = (struct channel_ref){.handle = le16(d + 8 + 3 * i),
                                                               .channel = m->channel_count};
            }
            m->channel_count++;
        }
    next:
        pos = end + 2;
    }
    return true;
}

static bool read_memory(const char *path, struct memory *m)
{
    FILE *f = fopen(path, "re");
    if (!f)
        return false;
    unsigned char *data = malloc(MAX_DMI_SIZE + 1);
    if (!data) {
        fclose(f);
        return false;
    }
    size_t size = fread(data, 1, MAX_DMI_SIZE + 1, f);
    bool ok = size && size <= MAX_DMI_SIZE && !ferror(f) && decode(data, size, m);
    free(data);
    fclose(f);
    return ok;
}

/* Type 37 is authoritative only if every populated Type 17 device is mapped
 * once. Two DIMMs on one channel share its bus, so use the slower speed once. */
static bool mapped_peak(struct memory *m, unsigned max_channels, struct dbw_peak_estimate *out)
{
    if (!m->channel_count)
        return false;
    for (unsigned r = 0; r < m->ref_count; r++) {
        unsigned i;
        for (i = 0; i < m->count; i++)
            if (m->dimms[i].handle == m->refs[r].handle)
                break;
        if (i == m->count || m->dimms[i].channel >= 0)
            return false;
        m->dimms[i].channel = (int)m->refs[r].channel;
    }
    unsigned rates[MAX_DIMMS] = {0};
    for (unsigned i = 0; i < m->count; i++) {
        const struct dimm *v = &m->dimms[i];
        if (!v->populated)
            continue;
        if (v->channel < 0)
            return false;
        unsigned *rate = &rates[v->channel];
        if (!*rate || v->rate < *rate)
            *rate = v->rate;
    }
    unsigned channels = 0;
    double peak = 0;
    for (unsigned i = 0; i < m->channel_count; i++)
        if (rates[i]) {
            channels++;
            peak += rates[i] * 1e6 * 8;
        }
    if (!channels || channels > max_channels)
        return false;
    out->channels = channels;
    out->bytes_per_sec = peak;
    return true;
}

void dbw_peak_estimate_rome(const char *dmi_path, unsigned max_channels,
                            struct dbw_peak_estimate *out)
{
    memset(out, 0, sizeof(*out));
    if (!max_channels || max_channels > MAX_DIMMS)
        return;
    struct memory m = {0};
    unsigned rate = ROME_RATE;
    bool assumed = false;
    if (read_memory(dmi_path, &m)) {
        for (unsigned i = 0; i < m.count; i++) {
            if (!m.dimms[i].populated)
                continue;
            out->dimms++;
            if (m.dimms[i].rate < rate)
                rate = m.dimms[i].rate;
            assumed |= m.dimms[i].assumed;
        }
    }
    if (out->dimms) {
        if (mapped_peak(&m, max_channels, out)) {
            strcpy(out->source, "smbios-channels");
            snprintf(out->detail, sizeof(out->detail),
                     "%u populated channels, %u DIMMs; %s; 64 data bits (ECC excluded)",
                     out->channels, out->dimms,
                     assumed ? "some speed/width values assumed" : "configured speeds");
            return;
        }
        out->channels = out->dimms < max_channels ? out->dimms : max_channels;
        strcpy(out->source, "smbios-rome");
        snprintf(out->detail, sizeof(out->detail),
                 "%u DIMMs; assuming balanced population across %u/%u Rome channels; "
                 "%u MT/s x 8 bytes (minimum %s speed)",
                 out->dimms, out->channels, max_channels, rate,
                 assumed ? "configured/rated/assumed" : "configured");
    } else {
        out->channels = max_channels;
        strcpy(out->source, "rome-spec");
        snprintf(out->detail, sizeof(out->detail),
                 "SMBIOS unavailable/incomplete; assuming all %u Rome channels populated "
                 "at DDR4-3200 x 8 bytes",
                 max_channels);
    }
    out->bytes_per_sec = out->channels * (double)rate * 1e6 * 8;
}
