#ifndef DBW_BACKEND_H
#define DBW_BACKEND_H
#include "dram_bw.h"
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>

#define DBW_MAX_COUNTERS 512
struct dbw_counter_value {
    uint64_t value, enabled, running;
};
struct dbw_counter {
    int fd;
    int cpu;
    char pmu[128];
    uint64_t config, config1, config2;
    unsigned kind; /* 0: read, 1: write, 2: total */
    double bytes;
    struct dbw_counter_value previous;
    bool baseline_valid;
    uint64_t previous_end_ns;
    struct {
        struct dbw_counter_value old, now;
        uint64_t start_ns, end_ns, previous_end_ns;
        uint32_t flags;
        int error;
        bool had_baseline;
    } observation;
};
struct dbw_backend {
    struct dbw_counter counters[DBW_MAX_COUNTERS];
    size_t count;
    bool mock, total_only;
    uint64_t previous_ns, sequence;
    uint64_t previous_scan_ns;
    double peak;
    FILE *diagnostics;
};
int dbw_backend_open(struct dbw_backend *b, const char *name, const char *events, double peak);
int dbw_backend_start(struct dbw_backend *b);
void dbw_backend_stop(struct dbw_backend *b);
void dbw_backend_close(struct dbw_backend *b);
void dbw_backend_sample(struct dbw_backend *b, dbw_sample *s, uint64_t interval_ns);
int dbw_backend_diagnostics_open(struct dbw_backend *b, const char *path);
/* Separate pure calculation for unit validation of multiplexing/scaling. */
double dbw_counter_rate(struct dbw_counter_value old, struct dbw_counter_value now, double bytes,
                        uint32_t *flags);
#endif
