#define _GNU_SOURCE
#include "internal.h"
#include "backend.h"
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <unistd.h>

static void ring_test(void)
{
    struct dbw_producer p;
    assert(dbw_producer_create(&p, 3, 1000, 0) == -1);
    assert(dbw_producer_create(&p, 4, 1000, 1e9) == 0);
    assert(ftruncate(p.cursor_fd, 0) == -1 && errno == EPERM);
    assert(ftruncate(p.data_fd, 0) == -1);
    void *bad = mmap(NULL, p.map_size, PROT_READ | PROT_WRITE, MAP_SHARED, p.data_fd, 0);
    assert(bad == MAP_FAILED);
    char path[64];
    snprintf(path, sizeof(path), "/proc/self/fd/%d", p.data_fd);
    int rw = open(path, O_RDWR | O_CLOEXEC);
    assert(rw >= 0);
    assert(pwrite(rw, "x", 1, 0) == -1 && errno == EPERM);
    bad = mmap(NULL, p.map_size, PROT_READ | PROT_WRITE, MAP_SHARED, rw, 0);
    assert(bad == MAP_FAILED && errno == EPERM);
    close(rw);
    for (uint64_t i = 0; i < 4; i++) {
        dbw_sample s = {.sequence = i};
        assert(dbw_producer_push(&p, &s) == 1);
    }
    dbw_sample s = {.sequence = 4};
    assert(dbw_producer_push(&p, &s) == 0);
    assert(atomic_load(&p.ring->dropped) == 1 && atomic_load(&p.ring->head) == 4);
    for (uint64_t i = 0; i < 4; i++)
        assert(p.samples[i].sequence == i);
    uint64_t event;
    assert(read(p.event_fd, &event, sizeof(event)) == -1 && errno == EAGAIN);
    atomic_store(&p.cursor->tail, 2);
    atomic_store(&p.cursor->waiting, 1);
    assert(dbw_producer_push(&p, &s) == 1);
    assert(read(p.event_fd, &event, sizeof(event)) == sizeof(event) && event == 1);
    assert(p.samples[0].sequence == 4 && p.samples[2].sequence == 2);
    atomic_store(&p.cursor->tail, UINT64_MAX);
    assert(dbw_producer_push(&p, &s) == -1 && errno == EPROTO);
    atomic_store(&p.cursor->tail, 1); /* Rewind is also rejected. */
    assert(dbw_producer_push(&p, &s) == -1 && errno == EPROTO);
    /* Exercise unsigned counter wrap. */
    p.head = p.tail = UINT64_MAX - 1;
    atomic_store(&p.ring->head, p.head);
    atomic_store(&p.cursor->tail, p.tail);
    for (int i = 0; i < 4; i++)
        assert(dbw_producer_push(&p, &s) == 1);
    assert(dbw_producer_push(&p, &s) == 0);
    atomic_store(&p.cursor->tail, p.head);
    assert(dbw_producer_push(&p, &s) == 1);
    dbw_producer_destroy(&p);
}

static void perf_math_test(void)
{
    uint32_t flags = 0;
    struct dbw_counter_value old = {100, 1000000000, 500000000};
    struct dbw_counter_value now = {200, 1200000000, 600000000};
    assert(dbw_counter_rate(old, now, 64, &flags) == 64000);
    assert(flags == DBW_SAMPLE_MULTIPLEXED);
    now.running = old.running;
    flags = 0;
    assert(isnan(dbw_counter_rate(old, now, 64, &flags)));
    assert(flags == (DBW_SAMPLE_INVALID | DBW_SAMPLE_NOT_RUNNING));
    now = (struct dbw_counter_value){200, 1200000000, 700000000};
    flags = 0;
    assert(dbw_counter_rate(old, now, 64, &flags) == 32000 && !flags);
    now.value = old.value; /* Idle hardware is valid, not an error. */
    assert(dbw_counter_rate(old, now, 64, &flags) == 0 && !flags);
    flags = 0;
    now = (struct dbw_counter_value){200, old.enabled, old.running};
    assert(isnan(dbw_counter_rate(old, now, 64, &flags)));
    assert(flags == (DBW_SAMPLE_INVALID | DBW_SAMPLE_NOT_ENABLED));
    flags = 0;
    now.enabled = old.enabled - 1;
    assert(isnan(dbw_counter_rate(old, now, 64, &flags)));
    assert(flags == (DBW_SAMPLE_INVALID | DBW_SAMPLE_TIME_BACKWARDS));
    flags = 0;
    old = (struct dbw_counter_value){UINT64_MAX - 5, 0, 0};
    now = (struct dbw_counter_value){4, 100000000, 100000000};
    assert(dbw_counter_rate(old, now, 64, &flags) == 6400 && !flags);
    now.enabled--;
    assert(isnan(dbw_counter_rate(old, now, 64, &flags)));
    assert(flags == (DBW_SAMPLE_INVALID | DBW_SAMPLE_RUNNING_GT_ENABLED));
}

static void feed_counter(struct dbw_counter *c, struct dbw_counter_value value)
{
    if (c->fd >= 0)
        close(c->fd);
    c->fd = memfd_create("perf-test-input", MFD_CLOEXEC);
    assert(c->fd >= 0);
    assert(write(c->fd, &value, sizeof(value)) == sizeof(value));
    assert(lseek(c->fd, 0, SEEK_SET) == 0);
}

static void snapshot_test(bool diagnostics)
{
    struct dbw_backend b = {.count = 2, .total_only = true};
    for (int i = 0; i < 2; i++) {
        b.counters[i] =
            (struct dbw_counter){.fd = -1, .kind = 2, .bytes = 64, .baseline_valid = true};
        strcpy(b.counters[i].pmu, "test");
    }
    /* Two windows with different lengths, representing equal per-channel
     * rates, while scan-end wall time is unrelated to either window. */
    feed_counter(&b.counters[0], (struct dbw_counter_value){100, 100000000, 50000000});
    feed_counter(&b.counters[1], (struct dbw_counter_value){200, 200000000, 100000000});
    b.previous_ns = dbw_now() - 240000000;
    b.previous_scan_ns = 140000000;
    if (diagnostics) {
        b.diagnostics = tmpfile();
        assert(b.diagnostics);
    }
    dbw_sample s;
    dbw_backend_sample(&b, &s, 100000000);
    assert(s.total_bytes_per_sec == 256000); /* No global wall-time divisor. */
    assert(!(s.flags & DBW_SAMPLE_INVALID));
    assert((s.flags & (DBW_SAMPLE_WINDOW_SKEW | DBW_SAMPLE_LATE | DBW_SAMPLE_PEAK_UNKNOWN)) ==
           (DBW_SAMPLE_WINDOW_SKEW | DBW_SAMPLE_LATE | DBW_SAMPLE_PEAK_UNKNOWN));
    assert(s.missed_ticks >= 1 && isnan(s.utilization));
    assert(isnan(s.read_bytes_per_sec) && isnan(s.write_bytes_per_sec));
    if (diagnostics) {
        rewind(b.diagnostics);
        char log[4096];
        size_t size = fread(log, 1, sizeof(log) - 1, b.diagnostics);
        log[size] = 0;
        assert(strstr(log, "\"delta\":[100,100000000,50000000]"));
        assert(strstr(log, "\"running_fraction\":0.5"));
        assert(strstr(log, "\"bytes_per_count\":64"));
    }
    dbw_backend_close(&b);

    memset(&b, 0, sizeof(b));
    b.count = 1;
    b.total_only = true;
    if (diagnostics) {
        b.diagnostics = tmpfile();
        assert(b.diagnostics);
    }
    b.counters[0] = (struct dbw_counter){.fd = -1, .kind = 2, .bytes = 64, .baseline_valid = true};
    strcpy(b.counters[0].pmu, "test");
    b.previous_ns = dbw_now() - 100000000;
    feed_counter(&b.counters[0], (struct dbw_counter_value){100, 100000000, 100000000});
    dbw_backend_sample(&b, &s, 100000000);
    assert(s.total_bytes_per_sec == 64000);
    /* EOF stands in for a short perf read. The next successful read must
     * establish a baseline, never turn the whole read gap into a spike. */
    dbw_backend_sample(&b, &s, 100000000);
    assert((s.flags & (DBW_SAMPLE_INVALID | DBW_SAMPLE_READ_ERROR)) ==
           (DBW_SAMPLE_INVALID | DBW_SAMPLE_READ_ERROR));
    if (diagnostics)
        assert(b.counters[0].observation.error == EIO);
    assert(isnan(s.total_bytes_per_sec));
    feed_counter(&b.counters[0], (struct dbw_counter_value){500, 500000000, 500000000});
    dbw_backend_sample(&b, &s, 100000000);
    assert(s.flags & DBW_SAMPLE_NO_BASELINE);
    assert(isnan(s.total_bytes_per_sec));
    feed_counter(&b.counters[0], (struct dbw_counter_value){600, 600000000, 600000000});
    b.peak = 128000;
    dbw_backend_sample(&b, &s, 100000000);
    assert(!(s.flags & DBW_SAMPLE_INVALID) && !(s.flags & DBW_SAMPLE_PEAK_UNKNOWN));
    assert(s.total_bytes_per_sec == 64000 && s.utilization == .5);
    dbw_backend_close(&b);
}

static void config_test(void)
{
    /* Invalid lists must be rejected before looking up a PMU or opening perf
     * FDs, even when an earlier line is valid. No hardware privilege needed. */
    const char *invalid[] = {
        "",
        "missing 0 total 0 0 0 64 extra\n",
        "missing 0 read 0 0 0 64\n",
        "missing 4294967296 total 0 0 0 64\n",
        "missing -4294967296 total 0 0 0 64\n",
        "missing 0 total 10000000000000000 0 0 64\n",
        "missing 0 total 0 -1 0 64\n",
        "missing 0 total 0 0 xyz 64\n",
        "missing 0 total 0 0 0 1e999\n",
        "missing 0 total 0 0 0 1e-999\n",
        "missing 0 total 0 0 0 nan\n",
        "missing 0 total 0 0 0 0\n",
        "missing 0 total 0 0 0 64\nmissing 0 total 0 0 0 32\n",
        "missing 0 read 0 0 0 64\nmissing 0 write 0 0 0 64\n",
        "missing 0 total 0 0 0 64\nmissing 0 read 1 0 0 64\n",
    };
    char path[] = "/tmp/dbw-config-XXXXXX";
    int fd = mkstemp(path);
    assert(fd >= 0);
    FILE *f = fdopen(fd, "w");
    assert(f);
    struct dbw_backend b;
    for (size_t i = 0; i < sizeof(invalid) / sizeof(*invalid); i++) {
        rewind(f);
        assert(ftruncate(fd, 0) == 0);
        assert(fputs(invalid[i], f) >= 0 && fflush(f) == 0);
        assert(dbw_backend_open(&b, "auto", path, 0) == -1 && errno == EINVAL);
        assert(!b.count);
    }
    rewind(f);
    assert(ftruncate(fd, 0) == 0);
    assert(fputs("missing 0 total 0 0 0 64 #", f) >= 0);
    for (unsigned i = 0; i < 1024; i++)
        assert(fputc('x', f) != EOF);
    assert(fflush(f) == 0);
    assert(dbw_backend_open(&b, "auto", path, 0) == -1 && errno == EINVAL);
    rewind(f);
    assert(ftruncate(fd, 0) == 0);
    assert(fputs("# Full-width configs and distinct config1/config2 values.\n"
                 "missing 2147483647 total 0xffffffffffffffff 0x1 0x2 64\n"
                 "missing 2147483647 total ffffffffffffffff 2 2 64\n"
                 "missing 2147483647 total ffffffffffffffff 2 3 64\n",
                 f) >= 0);
    assert(fflush(f) == 0);
    assert(dbw_backend_open(&b, "auto", path, 0) == -1 && errno == ENOENT);
    assert(b.counters[0].config == UINT64_MAX && b.counters[0].cpu == 2147483647);
    assert(b.counters[1].config1 == 2 && b.counters[2].config2 == 3);
    assert(!b.count);
    fclose(f);
    unlink(path);
}

static void ipc_test(void)
{
    int sockets[2];
    assert(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, sockets) == 0);
    struct dbw_msg msg = {.magic = DBW_MAGIC, .version = DBW_VERSION, .op = DBW_OPEN};
    int fd = open("/dev/null", O_RDONLY | O_CLOEXEC), received[3];
    assert(fd >= 0);
    assert(dbw_send_msg(sockets[0], &msg, &fd, 1) == 0);
    size_t n = 3;
    assert(dbw_recv_msg(sockets[1], &msg, received, &n) == 0 && n == 1);
    assert(fcntl(received[0], F_GETFD) & FD_CLOEXEC);
    close(received[0]);
    assert(dbw_send_msg(sockets[0], &msg, &fd, 1) == 0);
    n = 0;
    assert(dbw_recv_msg(sockets[1], &msg, NULL, &n) == -1 && errno == EPROTO);
    char large[256] = {0};
    memcpy(large, &msg, sizeof(msg));
    ssize_t sent = send(sockets[0], large, sizeof(large), 0);
    if (sent < 0)
        perror("oversized send");
    assert(sent == sizeof(large));
    assert(dbw_recv_msg(sockets[1], &msg, NULL, &n) == -1 && errno == EPROTO);
    close(fd);
    close(sockets[0]);
    close(sockets[1]);
}

static void usage(FILE *out)
{
    fprintf(out, "Usage: test-core [OPTIONS]\n"
                 "\n"
                 "Run unit tests for the ring, IPC, counter math, diagnostics and events.\n"
                 "No daemon or PMU permissions needed. Uses temporary files and local sockets.\n"
                 "Prints a success summary to stdout; failed assertions abort the process.\n"
                 "\n"
                 "Options:\n"
                 "  -h, --help    Show this help and exit without running tests.\n"
                 "\n"
                 "Build and run: make build/test-core && ./build/test-core\n"
                 "Full test suite: make test\n"
                 "Exit status: 0 = help or tests passed; 2 = usage error.\n");
}

int main(int argc, char **argv)
{
    const struct option options[] = {{"help", no_argument, NULL, 'h'}, {0}};
    int opt;
    while ((opt = getopt_long(argc, argv, "h", options, NULL)) != -1) {
        if (opt == 'h') {
            usage(stdout);
            return 0;
        }
        usage(stderr);
        return 2;
    }
    if (optind != argc) {
        usage(stderr);
        return 2;
    }
    ring_test();
    perf_math_test();
    snapshot_test(false);
    snapshot_test(true);
    config_test();
    ipc_test();
    puts("core: ring overflow/wrap, hostile cursor, memfd seals, per-counter rates, read recovery, "
         "diagnostics, event validation, IPC passed");
    return 0;
}
