#ifndef DRAM_BW_H
#define DRAM_BW_H

#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Linux 64-bit; client and daemon must use the same native ABI.
 *
 * Copying:   open -> start -> repeat(wait -> read_batch) -> close.
 * Zero-copy: open -> start -> repeat(wait -> peek -> consume) -> close.
 * Use stop to pause a subscription and start to resume it. wait is optional
 * for busy-polling readers. Neither waiting nor reading triggers sampling.
 *
 * One thread per handle; independent handles may be used concurrently.
 * Do not call these APIs from signal handlers.
 * Unless documented otherwise, success returns 0; errors return -1 with errno.
 */

#define DBW_SAMPLE_MOCK 1u          /* Synthetic data, not a hardware measurement. */
#define DBW_SAMPLE_INVALID 2u       /* Bandwidth fields are NaN; see reason bits below. */
#define DBW_SAMPLE_MULTIPLEXED 4u   /* Events shared PMU time; rates are scaled. */
#define DBW_SAMPLE_LATE 8u          /* At least one extra whole target period elapsed. */
#define DBW_SAMPLE_TOTAL_ONLY 16u   /* Read/write split unavailable; use total. */
#define DBW_SAMPLE_WINDOW_SKEW 32u  /* Current/previous scan > 10% of target period. */
#define DBW_SAMPLE_PEAK_UNKNOWN 64u /* Reference peak unavailable; utilization NaN. */
/* INVALID includes one or more of these reasons (OR across all counters). */
#define DBW_SAMPLE_READ_ERROR (1u << 8)
#define DBW_SAMPLE_NO_BASELINE (1u << 9)
#define DBW_SAMPLE_TIME_BACKWARDS (1u << 10)
#define DBW_SAMPLE_NOT_ENABLED (1u << 11)
#define DBW_SAMPLE_NOT_RUNNING (1u << 12)
#define DBW_SAMPLE_RUNNING_GT_ENABLED (1u << 13)
#define DBW_SAMPLE_ZERO_INTERVAL (1u << 14)
/* Per-handle subscription state, not the daemon's global sampling state. */
#define DBW_STOPPED 0u
#define DBW_RUNNING 1u
#define DBW_DEAD 2u

typedef struct dbw_client dbw_client;

/* Fixed 64-byte record of host-wide memory traffic, not per-process/container.
 * Rates are bytes/second. NaN means unknown/invalid, never zero traffic.
 * TOTAL_ONLY makes read/write NaN; INVALID makes all bandwidth fields NaN. */
typedef struct {
    uint64_t sequence;     /* Daemon-wide sample number; gaps are possible. */
    uint64_t timestamp_ns; /* Host CLOCK_MONOTONIC, interval end. */
    uint64_t interval_ns;  /* Wall time between scan ends; NOT rate denominator. */
    double read_bytes_per_sec;
    double write_bytes_per_sec;
    double total_bytes_per_sec;
    double utilization;    /* total / reference peak; may exceed 1; NaN if unknown. */
    uint32_t missed_ticks; /* Extra whole target periods elapsed; saturated. */
    uint32_t flags;        /* Bitwise OR of DBW_SAMPLE_* values. */
} dbw_sample;

/* A contiguous view into the ring, borrowed by dbw_peek(). */
typedef struct {
    const dbw_sample *samples;
    size_t count;
} dbw_span;

typedef struct {
    uint64_t produced;         /* Successfully queued records, including consumed. */
    uint64_t dropped;          /* New records discarded because this ring was full. */
    uint64_t available;        /* Queued records not yet consumed. */
    uint64_t interval_ns;      /* Minimum delay after each completed scan. */
    double peak_bytes_per_sec; /* Reference peak; 0 if not configured. */
    uint32_t capacity;         /* Ring capacity in records. */
    uint32_t state;            /* DBW_STOPPED, DBW_RUNNING or DBW_DEAD. */
} dbw_stats;

/* Connect to socket_path and allocate an independent ring; initially STOPPED.
 * out and socket_path must be non-NULL. capacity is a record count: a power of
 * two in [2, 65536], or 0 for 4096. Success stores the owned handle in *out.
 * A full ring drops new samples, increments dropped and leaves other clients
 * unaffected. open/start/stop use control sends/receives with 5-second timeouts.
 * After a control transport/protocol failure, close the handle and reopen it. */
int dbw_open(dbw_client **out, const char *socket_path, uint32_t capacity);

/* Subscribe at the daemon's configured interval; repeated start is a no-op.
 * The first active subscription enables sampling; it continues even without
 * reads. Joining ongoing sampling skips the already-started interval.
 * Previously queued records remain readable, including across stop/start. */
int dbw_start(dbw_client *client);

/* Synchronously unsubscribe; repeated stop is a no-op. Keeps the handle and
 * queued records for reading or a later start. The last active subscription
 * stopping or disconnecting disables the sampling timer and PMU events. */
int dbw_stop(dbw_client *client);

/* Release the handle and all borrowed views; NULL is allowed. No prior stop
 * is needed. The daemon reclaims the subscription when it detects disconnect.
 * Process exit (including SIGKILL) also closes the connection, unless another
 * process still holds an inherited/duplicated socket. An idle live connection
 * is not automatically reclaimed. */
void dbw_close(dbw_client *client);

/* Nonblocking data access: peek/consume/read_batch/get_stats perform no
 * syscalls, locking or allocation. Use wait to sleep or detect disconnect. */

/* Borrow up to max_count records without consuming them. Returns the total
 * count (0 if empty); spans[0] then spans[1] give the records in queue order.
 * spans must have two entries. Views are read-only and valid until consume or
 * close. Do not mix peek and read_batch before consuming the borrowed records.
 * max_count=0 returns 0 and clears the output spans without consuming data. */
ssize_t dbw_peek(dbw_client *client, dbw_span spans[2], size_t max_count);

/* Release the first count unread records from the last peek. count must not
 * exceed its remaining borrowed count; partial consumption is allowed.
 * count=0 is a no-op. Released records may immediately be reused by daemon. */
int dbw_consume(dbw_client *client, size_t count);

/* Copy and consume up to max_count records into caller-owned out. Returns the
 * copied count (0 if empty); no subsequent consume is needed. out must hold
 * max_count records, or may be NULL when max_count=0 (a no-op).
 * This does not report disconnect; drain queued data and check with wait. */
ssize_t dbw_read_batch(dbw_client *client, dbw_sample *out, size_t max_count);

/* Read this handle's statistics into out; fields are not an atomic snapshot.
 * Counts persist across stop/start. state alone cannot detect a daemon crash;
 * use wait for disconnect detection. */
int dbw_get_stats(dbw_client *client, dbw_stats *out);

/* Wait without consuming: 1 = data available; 0 = timeout or stopped with an
 * empty queue; -1 = error. timeout_ms: -1 waits indefinitely, 0 polls, >0 waits
 * up to that many milliseconds. Queued data takes priority over stop/disconnect:
 * read it first; once drained, disconnect returns -1 with errno=EPIPE.
 * Reconnect with a new handle after disconnect; there is no automatic retry. */
int dbw_wait(dbw_client *client, int timeout_ms);

#ifdef __cplusplus
}
#endif
#endif
