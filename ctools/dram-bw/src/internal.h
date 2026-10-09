#ifndef DBW_INTERNAL_H
#define DBW_INTERNAL_H
#include "dram_bw.h"
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <time.h>

#define DBW_MAGIC 0x44525742u
#define DBW_VERSION 1u
#define DBW_HEADER_SIZE 4096u
#define DBW_MAX_CAPACITY 65536u
#define DBW_MAX_CLIENTS 64
#define DBW_OPEN 1u
#define DBW_START 2u
#define DBW_STOP 3u

struct dbw_msg {
    uint32_t magic, version, op, capacity;
    int32_t error;
    uint32_t reserved;
};

struct dbw_ring {
    uint32_t magic, version, capacity, sample_size;
    uint64_t interval_ns;
    double peak_bytes_per_sec;
    _Alignas(64) _Atomic uint64_t head;
    _Alignas(64) _Atomic uint64_t dropped;
    _Atomic uint32_t state;
};

struct dbw_cursor {
    _Alignas(64) _Atomic uint64_t tail;
    _Alignas(64) _Atomic uint32_t waiting;
};

struct dbw_producer {
    struct dbw_ring *ring;
    struct dbw_cursor *cursor;
    dbw_sample *samples;
    size_t map_size;
    uint64_t head, tail;
    uint32_t capacity; /* Private: never trust a client's shared metadata. */
    int data_fd, cursor_fd, event_fd, notify_fd;
};

_Static_assert(sizeof(void *) == 8, "64-bit Linux required");
_Static_assert(ATOMIC_LLONG_LOCK_FREE == 2 && ATOMIC_LONG_LOCK_FREE == 2 &&
                   ATOMIC_INT_LOCK_FREE == 2,
               "lock-free shared atomics required");
_Static_assert(sizeof(dbw_sample) == 64, "sample ABI");
_Static_assert(sizeof(struct dbw_msg) == 24, "control ABI");
_Static_assert(sizeof(struct dbw_ring) <= DBW_HEADER_SIZE, "header ABI");

static inline uint64_t dbw_now(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

static inline bool dbw_capacity_valid(uint32_t n)
{
    return n >= 2 && n <= DBW_MAX_CAPACITY && !(n & (n - 1));
}

int dbw_producer_create(struct dbw_producer *p, uint32_t capacity, uint64_t interval_ns,
                        double peak);
void dbw_producer_destroy(struct dbw_producer *p);
int dbw_producer_push(struct dbw_producer *p, const dbw_sample *sample);
void dbw_producer_state(struct dbw_producer *p, uint32_t state);
int dbw_send_msg(int fd, const struct dbw_msg *msg, const int *fds, size_t nfds);
int dbw_recv_msg(int fd, struct dbw_msg *msg, int *fds, size_t *nfds);
#endif
