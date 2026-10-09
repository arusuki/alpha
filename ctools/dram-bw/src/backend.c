#define _GNU_SOURCE
#include "backend.h"
#include "internal.h"
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <limits.h>
#include <linux/perf_event.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>
#if defined(__x86_64__)
#include <cpuid.h>
#endif

#define PMU_ROOT "/sys/bus/event_source/devices"

static int read_text(const char *path, char *buf, size_t size)
{
    FILE *f = fopen(path, "re");
    if (!f)
        return -1;
    if (!fgets(buf, (int)size, f)) {
        fclose(f);
        errno = EIO;
        return -1;
    }
    fclose(f);
    return 0;
}

static int add_counter(struct dbw_backend *b, const char *pmu, int cpu, unsigned kind,
                       uint64_t config, uint64_t config1, uint64_t config2, double bytes)
{
    if (b->count == DBW_MAX_COUNTERS) {
        errno = E2BIG;
        return -1;
    }
    if (!*pmu || strlen(pmu) >= sizeof(b->counters[0].pmu) ||
        strspn(pmu, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") !=
            strlen(pmu)) {
        errno = EINVAL;
        return -1;
    }
    for (size_t i = 0; i < b->count; i++) {
        const struct dbw_counter *c = &b->counters[i];
        if (c->cpu == cpu && !strcmp(c->pmu, pmu) && c->config == config && c->config1 == config1 &&
            c->config2 == config2) {
            errno = EINVAL;
            return -1;
        }
    }
    struct dbw_counter *c = &b->counters[b->count++];
    *c = (struct dbw_counter){.fd = -1,
                              .cpu = cpu,
                              .config = config,
                              .config1 = config1,
                              .config2 = config2,
                              .kind = kind,
                              .bytes = bytes};
    strcpy(c->pmu, pmu);
    return 0;
}

static int open_counter(struct dbw_counter *c)
{
    char path[256], buf[128], *end;
    snprintf(path, sizeof(path), PMU_ROOT "/%s/type", c->pmu);
    if (read_text(path, buf, sizeof(buf)))
        return -1;
    errno = 0;
    unsigned long type = strtoul(buf, &end, 10);
    if (errno || end == buf || type > UINT32_MAX || (*end && *end != '\n')) {
        errno = EINVAL;
        return -1;
    }
    struct perf_event_attr attr = {.size = sizeof(attr),
                                   .type = (uint32_t)type,
                                   .config = c->config,
                                   .config1 = c->config1,
                                   .config2 = c->config2,
                                   .disabled = 1,
                                   .read_format = PERF_FORMAT_TOTAL_TIME_ENABLED |
                                                  PERF_FORMAT_TOTAL_TIME_RUNNING};
    /* Independent events: eight Rome channels must multiplex over four DF
     * counters. Putting all eight in one group would make it unschedulable. */
    c->fd = (int)syscall(SYS_perf_event_open, &attr, -1, c->cpu, -1, PERF_FLAG_FD_CLOEXEC);
    if (c->fd < 0) {
        int saved = errno;
        fprintf(stderr, "perf_event_open %s cpu=%d config=0x%" PRIx64 ": %s\n", c->pmu, c->cpu,
                c->config, strerror(saved));
        errno = saved;
        return -1;
    }
    return 0;
}

static bool is_rome(void)
{
#if defined(__x86_64__)
    unsigned a, b, c, d;
    if (!__get_cpuid(0, &a, &b, &c, &d))
        return false;
    if (b != 0x68747541 || d != 0x69746e65 || c != 0x444d4163)
        return false;
    if (!__get_cpuid(1, &a, &b, &c, &d))
        return false;
    unsigned family = (a >> 8) & 15;
    unsigned model = (a >> 4) & 15;
    if (family == 6 || family == 15)
        model += ((a >> 16) & 15) << 4;
    if (family == 15)
        family += (a >> 20) & 255;
    return family == 0x17 && model == 0x31;
#else
    return false;
#endif
}

static int open_rome(struct dbw_backend *b)
{
    if (!is_rome()) {
        errno = ENOTSUP;
        return -1;
    }
    char buf[4096];
    if (read_text(PMU_ROOT "/amd_df/cpumask", buf, sizeof(buf))) {
        fprintf(stderr, "amd_df PMU missing: check matching linux-modules-extra package and "
                        "amd_uncore module.\n");
        return -1;
    }
    /* Use all PMU representative CPUs, not all online cores (double counting).
     * Linux cpumask ABI uses CPU-list notation, including ranges. */
    char *save, *part = strtok_r(buf, ",\n", &save);
    if (!part) {
        errno = ENODEV;
        return -1;
    }
    while (part) {
        char *end;
        errno = 0;
        long first = strtol(part, &end, 10), last = first;
        if (end == part || errno || first < 0 || first > INT_MAX) {
            errno = EINVAL;
            return -1;
        }
        if (*end == '-') {
            char *start = end + 1;
            last = strtol(start, &end, 10);
            if (end == start) {
                errno = EINVAL;
                return -1;
            }
        }
        if (*end || errno || last < first || last > INT_MAX || last - first > 63) {
            errno = EINVAL;
            return -1;
        }
        for (long cpu = first; cpu <= last; cpu++) {
            for (unsigned channel = 0; channel < 8; channel++) {
                uint64_t event = 0x07 + channel * 0x40;
                uint64_t config = (event & 0xff) | ((event & 0xf00) << 24) | (0x38ull << 8);
                if (add_counter(b, "amd_df", (int)cpu, 2, config, 0, 0, 64))
                    return -1;
            }
        }
        part = strtok_r(NULL, ",\n", &save);
    }
    b->total_only = true;
    return 0;
}

static int open_file(struct dbw_backend *b, const char *path)
{
    FILE *f = fopen(path, "re");
    if (!f)
        return -1;
    char line[1024];
    unsigned lineno = 0, kinds = 0;
    while (fgets(line, sizeof(line), f)) {
        lineno++;
        if (strlen(line) == sizeof(line) - 1) {
            errno = EINVAL;
            goto fail;
        }
        char *comment = strchr(line, '#');
        if (comment)
            *comment = 0;
        char *fields[7], *save;
        size_t count = 0;
        for (char *p = strtok_r(line, " \t\r\n\v\f", &save); p;
             p = strtok_r(NULL, " \t\r\n\v\f", &save)) {
            if (count == 7) {
                errno = EINVAL;
                goto fail;
            }
            fields[count++] = p;
        }
        if (!count)
            continue;
        if (count != 7) {
            errno = EINVAL;
            goto fail;
        }
        uint64_t numbers[4];
        for (unsigned i = 0; i < 4; i++) {
            const char *p = fields[i ? i + 2 : 1];
            char *end;
            errno = 0;
            uintmax_t value = strtoumax(p, &end, i ? 16 : 10);
            if (errno || *p == '-' || *p == '+' || end == p || *end ||
                value > (i ? UINT64_MAX : (uintmax_t)INT_MAX)) {
                errno = EINVAL;
                goto fail;
            }
            numbers[i] = (uint64_t)value;
        }
        char *end;
        errno = 0;
        double bytes = strtod(fields[6], &end);
        if (errno || end == fields[6] || *end || !isfinite(bytes) || bytes <= 0) {
            errno = EINVAL;
            goto fail;
        }
        unsigned k;
        if (!strcmp(fields[2], "read"))
            k = 0;
        else if (!strcmp(fields[2], "write"))
            k = 1;
        else if (!strcmp(fields[2], "total"))
            k = 2;
        else {
            errno = EINVAL;
            goto fail;
        }
        kinds |= 1u << k;
        if (add_counter(b, fields[0], (int)numbers[0], k, numbers[1], numbers[2], numbers[3],
                        bytes))
            goto fail;
    }
    if (ferror(f)) {
        errno = EIO;
        goto fail;
    }
    /* Mixing total and read/write counters double-counts traffic. */
    if (kinds != 3 && kinds != 4) {
        errno = EINVAL;
        goto fail;
    }
    b->total_only = kinds == 4;
    fclose(f);
    return 0;
fail:;
    int saved = errno;
    fprintf(stderr, "%s:%u: invalid event configuration: %s\n", path, lineno, strerror(saved));
    fclose(f);
    errno = saved;
    return -1;
}

int dbw_backend_open(struct dbw_backend *b, const char *name, const char *events, double peak)
{
    memset(b, 0, sizeof(*b));
    b->peak = peak;
    if (!strcmp(name, "mock") && !events) {
        b->mock = true;
        return 0;
    }
    int ret;
    if (events && !strcmp(name, "auto"))
        ret = open_file(b, events);
    else if (!events && (!strcmp(name, "auto") || !strcmp(name, "amd-rome")))
        ret = open_rome(b);
    else {
        errno = EINVAL;
        ret = -1;
    }
    /* Validate the complete configuration before acquiring any perf FDs. */
    for (size_t i = 0; !ret && i < b->count; i++)
        ret = open_counter(&b->counters[i]);
    if (ret) {
        int saved = errno;
        dbw_backend_close(b);
        errno = saved;
    }
    return ret;
}

static int read_counter(int fd, struct dbw_counter_value *v)
{
    ssize_t n;
    do {
        n = read(fd, v, sizeof(*v));
    } while (n < 0 && errno == EINTR);
    if (n == sizeof(*v))
        return 0;
    if (n >= 0)
        errno = EIO;
    return -1;
}

int dbw_backend_start(struct dbw_backend *b)
{
    /* Enable every event before taking baselines, so activation latency of
     * later channels is not included in earlier channels' first window. */
    for (size_t i = 0; i < b->count; i++) {
        b->counters[i].baseline_valid = false;
        if (ioctl(b->counters[i].fd, PERF_EVENT_IOC_ENABLE, 0))
            goto fail;
    }
    uint64_t start_ns = dbw_now();
    for (size_t i = 0; i < b->count; i++) {
        struct dbw_counter *c = &b->counters[i];
        if (read_counter(c->fd, &c->previous))
            goto fail;
        if (b->diagnostics)
            c->previous_end_ns = dbw_now();
        c->baseline_valid = true;
    }
    b->previous_ns = dbw_now();
    b->previous_scan_ns = b->previous_ns - start_ns;
    return 0;
fail:;
    int saved = errno;
    dbw_backend_stop(b);
    errno = saved;
    return -1;
}

void dbw_backend_stop(struct dbw_backend *b)
{
    for (size_t i = 0; i < b->count; i++) {
        ioctl(b->counters[i].fd, PERF_EVENT_IOC_DISABLE, 0);
        b->counters[i].baseline_valid = false;
    }
}

void dbw_backend_close(struct dbw_backend *b)
{
    for (size_t i = 0; i < b->count; i++)
        if (b->counters[i].fd >= 0)
            close(b->counters[i].fd);
    b->count = 0;
    if (b->diagnostics)
        fclose(b->diagnostics);
    b->diagnostics = NULL;
}

int dbw_backend_diagnostics_open(struct dbw_backend *b, const char *path)
{
    /* Append across restarts. Do not follow symlinks or block on a FIFO when
     * this path is opened by the privileged daemon. */
    int fd = open(path, O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK, 0640);
    if (fd < 0)
        return -1;
    struct stat st;
    if (fstat(fd, &st)) {
        int saved = errno;
        close(fd);
        errno = saved;
        return -1;
    }
    if (!S_ISREG(st.st_mode)) {
        close(fd);
        errno = EINVAL;
        return -1;
    }
    FILE *f = fdopen(fd, "a");
    if (!f) {
        int saved = errno;
        close(fd);
        errno = saved;
        return -1;
    }
    b->diagnostics = f;
    fprintf(f,
            "{\"type\":\"session\",\"pid\":%ld,\"counters\":%zu,\"mock\":%s,"
            "\"rate_formula\":\"delta_count*bytes_per_count*1e9/delta_running_ns\","
            "\"peak_bytes_per_sec\":%.17g}\n",
            (long)getpid(), b->count, b->mock ? "true" : "false", b->peak);
    if (fflush(f)) {
        int saved = errno;
        fclose(f);
        b->diagnostics = NULL;
        errno = saved;
        return -1;
    }
    return 0;
}

double dbw_counter_rate(struct dbw_counter_value old, struct dbw_counter_value now, double bytes,
                        uint32_t *flags)
{
    uint64_t enabled = now.enabled - old.enabled, running = now.running - old.running;
    uint32_t reason = 0;
    if (now.enabled < old.enabled || now.running < old.running)
        reason = DBW_SAMPLE_TIME_BACKWARDS;
    else if (!enabled)
        reason = DBW_SAMPLE_NOT_ENABLED;
    else if (!running)
        reason = DBW_SAMPLE_NOT_RUNNING;
    else if (running > enabled)
        reason = DBW_SAMPLE_RUNNING_GT_ENABLED;
    if (reason) {
        *flags |= DBW_SAMPLE_INVALID | reason;
        return NAN;
    }
    if (running < enabled)
        *flags |= DBW_SAMPLE_MULTIPLEXED;
    /* estimated_count = delta_count * delta_enabled / delta_running;
     * rate = estimated_count / delta_enabled_seconds. Each event has its own
     * enabled window. The enabled terms cancel: do NOT divide by the duration
     * between scan ends, which need not match this event's observation window. */
    return (double)(now.value - old.value) * bytes * 1e9 / (double)running;
}

static const char *reason_name(uint32_t flags)
{
    if (flags & DBW_SAMPLE_READ_ERROR)
        return "read_error";
    if (flags & DBW_SAMPLE_NO_BASELINE)
        return "no_baseline";
    if (flags & DBW_SAMPLE_TIME_BACKWARDS)
        return "time_backwards";
    if (flags & DBW_SAMPLE_NOT_ENABLED)
        return "not_enabled";
    if (flags & DBW_SAMPLE_NOT_RUNNING)
        return "not_running";
    if (flags & DBW_SAMPLE_RUNNING_GT_ENABLED)
        return "running_gt_enabled";
    return "ok";
}

static void write_diagnostics(struct dbw_backend *b, const dbw_sample *s, uint64_t interval_ns,
                              uint64_t scan_ns)
{
    FILE *f = b->diagnostics;
    if (!f)
        return;
    /* All I/O is AFTER snapshots, so formatting/flush cannot skew this scan. */
    for (size_t i = 0; i < b->count; i++) {
        const struct dbw_counter *c = &b->counters[i];
        const struct dbw_counter_value *old = &c->observation.old, *now = &c->observation.now;
        fprintf(f,
                "{\"type\":\"counter\",\"sequence\":%" PRIu64 ",\"index\":%zu,"
                "\"pmu\":\"%s\",\"cpu\":%d,\"config\":\"0x%" PRIx64 "\","
                "\"config1\":\"0x%" PRIx64 "\",\"config2\":\"0x%" PRIx64 "\","
                "\"bytes_per_count\":%.17g,"
                "\"had_baseline\":%s,\"previous_read_end_ns\":%" PRIu64 ","
                "\"read_start_ns\":%" PRIu64 ",\"read_end_ns\":%" PRIu64 ","
                "\"previous\":[%" PRIu64 ",%" PRIu64 ",%" PRIu64 "],"
                "\"current\":[%" PRIu64 ",%" PRIu64 ",%" PRIu64 "],"
                "\"flags\":%u,\"reason\":\"%s\",\"errno\":%d,\"delta\":",
                s->sequence, i, c->pmu, c->cpu, c->config, c->config1, c->config2, c->bytes,
                c->observation.had_baseline ? "true" : "false", c->observation.previous_end_ns,
                c->observation.start_ns, c->observation.end_ns, old->value, old->enabled,
                old->running, now->value, now->enabled, now->running, c->observation.flags,
                reason_name(c->observation.flags), c->observation.error);
        if (c->observation.had_baseline && !c->observation.error && now->enabled >= old->enabled &&
            now->running >= old->running) {
            uint64_t enabled = now->enabled - old->enabled, running = now->running - old->running;
            fprintf(f, "[%" PRIu64 ",%" PRIu64 ",%" PRIu64 "],\"running_fraction\":",
                    now->value - old->value, enabled, running);
            if (enabled)
                fprintf(f, "%.9g", (double)running / (double)enabled);
            else
                fputs("null", f);
        } else
            fputs("null,\"running_fraction\":null", f);
        fputs("}\n", f);
    }
    fprintf(f,
            "{\"type\":\"sample\",\"sequence\":%" PRIu64 ",\"timestamp_ns\":%" PRIu64
            ",\"interval_ns\":%" PRIu64 ",\"target_delay_ns\":%" PRIu64 ",\"scan_ns\":%" PRIu64
            ",\"previous_scan_ns\":%" PRIu64
            ",\"flags\":%u,\"missed_ticks\":%u,\"total_bytes_per_sec\":",
            s->sequence, s->timestamp_ns, s->interval_ns, interval_ns, scan_ns, b->previous_scan_ns,
            s->flags, s->missed_ticks);
    if (isfinite(s->total_bytes_per_sec))
        fprintf(f, "%.17g", s->total_bytes_per_sec);
    else
        fputs("null", f);
    fputs("}\n", f);
    if (fflush(f) || ferror(f)) {
        fprintf(stderr, "diagnostics write failed; disabling diagnostic log\n");
        fclose(f);
        b->diagnostics = NULL;
    }
}

void dbw_backend_sample(struct dbw_backend *b, dbw_sample *s, uint64_t interval_ns)
{
    memset(s, 0, sizeof(*s));
    s->sequence = ++b->sequence;
    uint64_t scan_start = dbw_now();
    double rates[3] = {0};
    if (b->mock) {
        s->flags |= DBW_SAMPLE_MOCK;
        rates[0] = 1e9;
        rates[1] = 5e8;
    } else
        for (size_t i = 0; i < b->count; i++) {
            struct dbw_counter *c = &b->counters[i];
            struct dbw_counter_value now = {0};
            uint32_t flags = 0;
            uint64_t start_ns = b->diagnostics ? dbw_now() : 0;
            int rc = read_counter(c->fd, &now);
            if (b->diagnostics) {
                c->observation.error = rc ? errno : 0;
                c->observation.start_ns = start_ns;
                c->observation.end_ns = dbw_now();
                c->observation.old = c->previous;
                c->observation.now = now;
                c->observation.had_baseline = c->baseline_valid;
                c->observation.previous_end_ns = c->previous_end_ns;
            }
            if (rc) {
                flags = DBW_SAMPLE_INVALID | DBW_SAMPLE_READ_ERROR;
                c->baseline_valid = false;
            } else {
                if (!c->baseline_valid)
                    flags = DBW_SAMPLE_INVALID | DBW_SAMPLE_NO_BASELINE;
                else
                    rates[c->kind] += dbw_counter_rate(c->previous, now, c->bytes, &flags);
                /* A successful read always establishes a fresh baseline. Do not
                 * silently blend a recovery interval into the next short sample. */
                c->previous = now;
                if (b->diagnostics)
                    c->previous_end_ns = c->observation.end_ns;
                c->baseline_valid = true;
            }
            if (b->diagnostics)
                c->observation.flags = flags;
            s->flags |= flags;
        }
    s->timestamp_ns = dbw_now();
    s->interval_ns = s->timestamp_ns - b->previous_ns;
    uint64_t scan_ns = s->timestamp_ns - scan_start;
    if (!s->interval_ns)
        s->flags |= DBW_SAMPLE_INVALID | DBW_SAMPLE_ZERO_INTERVAL;
    if (interval_ns) {
        uint64_t periods = s->interval_ns / interval_ns;
        uint64_t missed = periods > 0 ? periods - 1 : 0;
        s->missed_ticks = missed > UINT32_MAX ? UINT32_MAX : (uint32_t)missed;
        if (missed)
            s->flags |= DBW_SAMPLE_LATE;
        if (scan_ns > interval_ns / 10 || b->previous_scan_ns > interval_ns / 10)
            s->flags |= DBW_SAMPLE_WINDOW_SKEW;
    }
    if (b->total_only) {
        s->flags |= DBW_SAMPLE_TOTAL_ONLY;
        s->read_bytes_per_sec = s->write_bytes_per_sec = NAN;
        s->total_bytes_per_sec = rates[2];
    } else {
        s->read_bytes_per_sec = rates[0];
        s->write_bytes_per_sec = rates[1];
        s->total_bytes_per_sec = rates[0] + rates[1];
    }
    if (s->flags & DBW_SAMPLE_INVALID)
        s->read_bytes_per_sec = s->write_bytes_per_sec = s->total_bytes_per_sec = NAN;
    if (b->peak <= 0)
        s->flags |= DBW_SAMPLE_PEAK_UNKNOWN;
    s->utilization = b->peak > 0 ? s->total_bytes_per_sec / b->peak : NAN;
    write_diagnostics(b, s, interval_ns, scan_ns);
    b->previous_ns = s->timestamp_ns;
    b->previous_scan_ns = scan_ns;
}
