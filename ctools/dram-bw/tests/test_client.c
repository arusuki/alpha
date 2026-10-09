#define _GNU_SOURCE
#include "dram_bw.h"
#include <assert.h>
#include <errno.h>
#include <getopt.h>
#include <math.h>
#include <stdio.h>
#include <unistd.h>

static void usage(FILE *out)
{
    fprintf(out, "Usage: test-client [OPTIONS] SOCKET\n"
                 "\n"
                 "Test client start/stop, batch/zero-copy reads and slow-consumer isolation.\n"
                 "Requires a running mock daemon with --interval-us 1000 --peak-gbps 3.\n"
                 "Use make test to build and manage the required daemon automatically.\n"
                 "This is an assertion-based test, not a bandwidth display client.\n"
                 "\n"
                 "Arguments:\n"
                 "  SOCKET        Unix socket of the prepared mock daemon (required).\n"
                 "\n"
                 "Options:\n"
                 "  -h, --help    Show this help and exit without connecting.\n"
                 "  --            End options.\n"
                 "\n"
                 "Manual example (create /tmp/dram-bw-test first; use two terminals):\n"
                 "  ./build/dram-bwd --backend mock --interval-us 1000 --peak-gbps 3 --socket "
                 "/tmp/dram-bw-test/control.sock\n"
                 "  ./build/test-client /tmp/dram-bw-test/control.sock\n"
                 "\n"
                 "Output: success summary on stdout; failed assertions abort the process.\n"
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
    if (argc - optind != 1) {
        usage(stderr);
        return 2;
    }
    const char *path = argv[optind];
    dbw_client *c, *slow;
    assert(dbw_open(&c, path, 3) == -1 && errno == EINVAL);
    assert(dbw_open(&c, path, 8) == 0);
    assert(dbw_open(&slow, path, 2) == 0);
    assert(dbw_wait(c, 0) == 0);
    assert(dbw_start(c) == 0 && dbw_start(c) == 0);
    assert(dbw_start(slow) == 0);
    uint64_t previous = 0;
    for (int i = 0; i < 100; i++) {
        assert(dbw_wait(c, 1000) == 1);
        dbw_span spans[2];
        ssize_t n = dbw_peek(c, spans, 8);
        assert(n > 0 && n <= 8);
        assert(dbw_consume(c, (size_t)n + 1) == -1 && errno == EINVAL);
        for (int k = 0; k < 2; k++)
            for (size_t j = 0; j < spans[k].count; j++) {
                const dbw_sample *s = &spans[k].samples[j];
                assert(s->sequence > previous);
                previous = s->sequence;
                assert(s->flags & DBW_SAMPLE_MOCK);
                assert(fabs(s->total_bytes_per_sec - 1.5e9) < 1e-3);
                assert(fabs(s->utilization - .5) < 1e-12);
            }
        assert(dbw_consume(c, (size_t)n) == 0);
    }
    dbw_stats stats;
    assert(dbw_get_stats(slow, &stats) == 0 && stats.available == 2 && stats.dropped > 0);
    assert(dbw_stop(c) == 0 && dbw_stop(c) == 0);
    assert(dbw_get_stats(c, &stats) == 0 && stats.state == DBW_STOPPED);
    uint64_t produced = stats.produced;
    usleep(30000);
    assert(dbw_get_stats(c, &stats) == 0 && stats.produced == produced);
    dbw_sample batch[8];
    assert(dbw_read_batch(c, batch, 8) >= 0);
    assert(dbw_read_batch(c, NULL, 0) == 0);
    assert(dbw_wait(c, 10) == 0);
    assert(dbw_start(c) == 0 && dbw_wait(c, 1000) == 1);
    assert(dbw_stop(slow) == 0);
    assert(dbw_read_batch(slow, batch, 8) == 2);
    dbw_close(slow);
    assert(dbw_stop(c) == 0);
    dbw_close(c);
    puts("client: start/stop, zero-copy, batch, slow consumer isolation, restart passed");
    return 0;
}
