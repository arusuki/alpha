#define _GNU_SOURCE
#include "internal.h"
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <unistd.h>

static void wake_waiter(struct dbw_producer *p)
{
    /* The seq_cst load follows head/state publication and pairs with the
     * waiter's arm + recheck. Busy consumers need no write to this cache line. */
    if (atomic_load_explicit(&p->cursor->waiting, memory_order_seq_cst) &&
        atomic_exchange_explicit(&p->cursor->waiting, 0, memory_order_seq_cst)) {
        uint64_t one = 1;
        ssize_t n;
        do {
            n = send(p->notify_fd, &one, sizeof(one), MSG_DONTWAIT | MSG_NOSIGNAL);
        } while (n < 0 && errno == EINTR);
        /* EAGAIN means the notification queue is already readable. Separate
         * socket endpoints prevent a peer's F_SETFL from blocking our writes. */
    }
}

void dbw_producer_state(struct dbw_producer *p, uint32_t state)
{
    atomic_store_explicit(&p->ring->state, state, memory_order_seq_cst);
    wake_waiter(p);
}

int dbw_producer_create(struct dbw_producer *p, uint32_t capacity, uint64_t interval_ns,
                        double peak)
{
    memset(p, 0, sizeof(*p));
    p->data_fd = p->cursor_fd = p->event_fd = p->notify_fd = -1;
    if (!dbw_capacity_valid(capacity)) {
        errno = EINVAL;
        return -1;
    }
    p->capacity = capacity;
    p->map_size = DBW_HEADER_SIZE + (size_t)capacity * sizeof(dbw_sample);
    int fd = memfd_create("dram-bw-data", MFD_CLOEXEC | MFD_ALLOW_SEALING);
    if (fd < 0)
        return -1;
    p->data_fd = fd;
    if (ftruncate(fd, (off_t)p->map_size))
        goto fail;
    void *mem = mmap(NULL, p->map_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (mem == MAP_FAILED)
        goto fail;
    p->ring = mem;
    memset(mem, 0, p->map_size); /* Fault in pages before sampling. */
    p->samples = (dbw_sample *)((char *)mem + DBW_HEADER_SIZE);
    p->ring->magic = DBW_MAGIC;
    p->ring->version = DBW_VERSION;
    p->ring->capacity = capacity;
    p->ring->sample_size = sizeof(dbw_sample);
    p->ring->interval_ns = interval_ns;
    p->ring->peak_bytes_per_sec = peak;
    atomic_init(&p->ring->head, 0);
    atomic_init(&p->ring->dropped, 0);
    atomic_init(&p->ring->state, DBW_STOPPED);
    /* Existing producer mapping remains writable. No future writable mapping,
     * pwrite or resizing is allowed, even by a peer reopening /proc/self/fd. */
    if (fcntl(fd, F_ADD_SEALS, F_SEAL_GROW | F_SEAL_SHRINK | F_SEAL_FUTURE_WRITE | F_SEAL_SEAL) < 0)
        goto fail;
    char path[64];
    snprintf(path, sizeof(path), "/proc/self/fd/%d", fd);
    int ro = open(path, O_RDONLY | O_CLOEXEC);
    if (ro < 0)
        goto fail;
    close(fd);
    p->data_fd = ro;
    p->cursor_fd = memfd_create("dram-bw-cursor", MFD_CLOEXEC | MFD_ALLOW_SEALING);
    if (p->cursor_fd < 0 || ftruncate(p->cursor_fd, DBW_HEADER_SIZE))
        goto fail;
    mem = mmap(NULL, DBW_HEADER_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, p->cursor_fd, 0);
    if (mem == MAP_FAILED)
        goto fail;
    p->cursor = mem;
    memset(mem, 0, DBW_HEADER_SIZE);
    atomic_init(&p->cursor->tail, 0);
    atomic_init(&p->cursor->waiting, 0);
    if (fcntl(p->cursor_fd, F_ADD_SEALS, F_SEAL_GROW | F_SEAL_SHRINK | F_SEAL_SEAL) < 0)
        goto fail;
    int pair[2];
    if (socketpair(AF_UNIX, SOCK_DGRAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0, pair))
        goto fail;
    p->notify_fd = pair[0];
    p->event_fd = pair[1];
    return 0;
fail:;
    int saved = errno;
    dbw_producer_destroy(p);
    errno = saved;
    return -1;
}

void dbw_producer_destroy(struct dbw_producer *p)
{
    if (p->ring && p->cursor && p->event_fd >= 0)
        dbw_producer_state(p, DBW_DEAD);
    if (p->ring)
        munmap(p->ring, p->map_size);
    if (p->cursor)
        munmap(p->cursor, DBW_HEADER_SIZE);
    if (p->data_fd >= 0)
        close(p->data_fd);
    if (p->cursor_fd >= 0)
        close(p->cursor_fd);
    if (p->event_fd >= 0)
        close(p->event_fd);
    if (p->notify_fd >= 0)
        close(p->notify_fd);
    memset(p, 0, sizeof(*p));
    p->data_fd = p->cursor_fd = p->event_fd = p->notify_fd = -1;
}

int dbw_producer_push(struct dbw_producer *p, const dbw_sample *sample)
{
    uint64_t tail = atomic_load_explicit(&p->cursor->tail, memory_order_acquire);
    /* Unsigned distances also work across the 64-bit sequence wrap. */
    if (tail - p->tail > p->head - p->tail) {
        errno = EPROTO;
        return -1;
    }
    p->tail = tail;
    if (p->head - tail == p->capacity) {
        atomic_fetch_add_explicit(&p->ring->dropped, 1, memory_order_relaxed);
        wake_waiter(p);
        return 0;
    }
    p->samples[p->head & (p->capacity - 1)] = *sample;
    /* seq_cst is release plus the ordering needed for optional notification. */
    atomic_store_explicit(&p->ring->head, ++p->head, memory_order_seq_cst);
    wake_waiter(p);
    return 1;
}
