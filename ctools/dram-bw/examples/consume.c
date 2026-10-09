#include "dram_bw.h"
#include <errno.h>
#include <getopt.h>
#include <inttypes.h>
#include <stdio.h>
#include <stdlib.h>

static void usage(FILE *out)
{
    fprintf(out,
            "Usage: dram-bw-consume [OPTIONS] SOCKET [NUMBER_OF_SAMPLES]\n"
            "\n"
            "Client: subscribe to an existing dram-bwd, print samples, then exit.\n"
            "No PMU permissions are needed; access to the daemon socket is required.\n"
            "\n"
            "Arguments:\n"
            "  SOCKET                Unix socket served by dram-bwd (required).\n"
            "  NUMBER_OF_SAMPLES     Records to read, including invalid samples.\n"
            "                        Default: 100; range: 1..10000000.\n"
            "\n"
            "Options:\n"
            "  -h, --help            Show this help and exit without connecting.\n"
            "  --                    End options (for a socket path starting with '-').\n"
            "\n"
            "Output: CSV on stdout; diagnostics and final statistics on stderr.\n"
            "CSV columns: sequence,host_monotonic_ns,interval_ns,read_GBps,write_GBps,\n"
            "             total_GBps,utilization,flags,missed_ticks\n"
            "GB/s = 1e9 bytes/s; utilization is a ratio (0.5 = 50%%).\n"
            "NaN means unknown/invalid, not zero. Rome supplies total bandwidth only.\n"
            "The daemon sets the sampling interval; this client does not change it.\n"
            "\n"
            "Examples:\n"
            "  dram-bw-consume /run/dram-bw/control.sock 10\n"
            "  dram-bw-consume /run/dram-bw/control.sock 100 > samples.csv\n"
            "\n"
            "Exit status: 0 = help or completed read; 1 = runtime failure; 2 = usage error.\n");
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
    if (argc - optind < 1 || argc - optind > 2) {
        usage(stderr);
        return 2;
    }
    size_t limit = 100;
    if (argc - optind == 2) {
        const char *count = argv[optind + 1];
        char *end;
        errno = 0;
        unsigned long n = strtoul(count, &end, 10);
        if (errno || *count < '0' || *count > '9' || *end || !n || n > 10000000) {
            fprintf(stderr, "NUMBER_OF_SAMPLES must be an integer in 1..10000000.\n");
            usage(stderr);
            return 2;
        }
        limit = n;
    }
    dbw_client *c;
    if (dbw_open(&c, argv[optind], 4096)) {
        perror("dbw_open");
        return 1;
    }
    if (dbw_start(c)) {
        perror("dbw_start");
        dbw_close(c);
        return 1;
    }
    dbw_stats stats;
    dbw_get_stats(c, &stats);
    if (stats.peak_bytes_per_sec <= 0)
        fprintf(stderr, "No reference peak: reporting bandwidth; utilization column is NaN.\n");
    printf("sequence,host_monotonic_ns,interval_ns,read_GBps,write_GBps,total_GBps,utilization,"
           "flags,missed_ticks\n");
    size_t consumed = 0;
    size_t invalid = 0, multiplexed = 0, skewed = 0;
    uint32_t invalid_reasons = 0;
    int result = 0;
    while (consumed < limit) {
        if (dbw_wait(c, 5000) < 0) {
            perror("dbw_wait");
            result = 1;
            break;
        }
        dbw_sample batch[256];
        size_t max = limit - consumed < 256 ? limit - consumed : 256;
        ssize_t n = dbw_read_batch(c, batch, max);
        if (n < 0) {
            perror("dbw_read_batch");
            result = 1;
            break;
        }
        for (ssize_t i = 0; i < n; i++) {
            const dbw_sample *s = &batch[i];
            if (s->flags & DBW_SAMPLE_INVALID) {
                invalid++;
                invalid_reasons |= s->flags & 0x7f00u;
            }
            if (s->flags & DBW_SAMPLE_MULTIPLEXED)
                multiplexed++;
            if (s->flags & DBW_SAMPLE_WINDOW_SKEW)
                skewed++;
            printf("%" PRIu64 ",%" PRIu64 ",%" PRIu64 ",%.6f,%.6f,%.6f,%.6f,%u,%u\n", s->sequence,
                   s->timestamp_ns, s->interval_ns, s->read_bytes_per_sec / 1e9,
                   s->write_bytes_per_sec / 1e9, s->total_bytes_per_sec / 1e9, s->utilization,
                   s->flags, s->missed_ticks);
        }
        consumed += (size_t)n;
    }
    dbw_get_stats(c, &stats);
    fprintf(stderr,
            "consumed=%zu valid=%zu invalid=%zu dropped=%" PRIu64
            " multiplexed=%zu skewed=%zu invalid_reason_bits=0x%x\n",
            consumed, consumed - invalid, invalid, stats.dropped, multiplexed, skewed,
            invalid_reasons);
    if (dbw_stop(c) && !result) {
        perror("dbw_stop");
        result = 1;
    }
    dbw_close(c);
    return result;
}
