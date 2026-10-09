#define _GNU_SOURCE
#include "internal.h"
#include <errno.h>
#include <poll.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

struct dbw_client {
    int socket_fd, event_fd;
    const struct dbw_ring *ring;
    struct dbw_cursor *cursor;
    const dbw_sample *samples;
    size_t map_size, borrowed;
    uint64_t tail;
    uint32_t capacity;
};

static int command(dbw_client *c, uint32_t op)
{
    if (!c) {
        errno = EINVAL;
        return -1;
    }
    struct dbw_msg m = {.magic = DBW_MAGIC, .version = DBW_VERSION, .op = op};
    size_t nfds = 0;
    if (dbw_send_msg(c->socket_fd, &m, NULL, 0) || dbw_recv_msg(c->socket_fd, &m, NULL, &nfds))
        goto broken;
    if (m.op != op || m.error < 0) {
        errno = EPROTO;
        goto broken;
    }
    if (m.error) {
        errno = m.error;
        return -1;
    }
    return 0;
broken:;
    int saved = errno;
    /* A timed-out command may still execute: forbid response misassociation. */
    shutdown(c->socket_fd, SHUT_RDWR);
    errno = saved;
    return -1;
}

int dbw_open(dbw_client **out, const char *path, uint32_t capacity)
{
    if (!out || !path) {
        errno = EINVAL;
        return -1;
    }
    *out = NULL;
    if (!capacity)
        capacity = 4096;
    if (!dbw_capacity_valid(capacity)) {
        errno = EINVAL;
        return -1;
    }
    struct sockaddr_un addr = {.sun_family = AF_UNIX};
    if (strlen(path) >= sizeof(addr.sun_path)) {
        errno = ENAMETOOLONG;
        return -1;
    }
    strcpy(addr.sun_path, path);
    dbw_client *c = calloc(1, sizeof(*c));
    if (!c)
        return -1;
    c->socket_fd = c->event_fd = -1;
    int fds[3] = {-1, -1, -1};
    c->socket_fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
    if (c->socket_fd < 0)
        goto fail;
    struct timeval timeout = {.tv_sec = 5};
    if (setsockopt(c->socket_fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) ||
        setsockopt(c->socket_fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)) ||
        connect(c->socket_fd, (struct sockaddr *)&addr, sizeof(addr)))
        goto fail;
    struct dbw_msg msg = {
        .magic = DBW_MAGIC, .version = DBW_VERSION, .op = DBW_OPEN, .capacity = capacity};
    size_t nfds = 3;
    if (dbw_send_msg(c->socket_fd, &msg, NULL, 0) || dbw_recv_msg(c->socket_fd, &msg, fds, &nfds))
        goto fail;
    if (msg.error > 0) {
        errno = msg.error;
        goto fail;
    }
    if (msg.error || msg.op != DBW_OPEN || msg.capacity != capacity || nfds != 3) {
        errno = EPROTO;
        goto fail;
    }
    c->capacity = capacity;
    c->map_size = DBW_HEADER_SIZE + (size_t)capacity * sizeof(dbw_sample);
    struct stat st;
    if (fstat(fds[0], &st))
        goto fail;
    if (st.st_size != (off_t)c->map_size) {
        errno = EPROTO;
        goto fail;
    }
    if (fstat(fds[1], &st))
        goto fail;
    if (st.st_size != DBW_HEADER_SIZE) {
        errno = EPROTO;
        goto fail;
    }
    void *mem = mmap(NULL, c->map_size, PROT_READ, MAP_SHARED, fds[0], 0);
    if (mem == MAP_FAILED)
        goto fail;
    c->ring = mem;
    if (c->ring->magic != DBW_MAGIC || c->ring->version != DBW_VERSION ||
        c->ring->capacity != capacity || c->ring->sample_size != sizeof(dbw_sample)) {
        errno = EPROTO;
        goto fail;
    }
    c->samples = (const dbw_sample *)((const char *)mem + DBW_HEADER_SIZE);
    mem = mmap(NULL, DBW_HEADER_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fds[1], 0);
    if (mem == MAP_FAILED)
        goto fail;
    c->cursor = mem;
    c->event_fd = fds[2];
    close(fds[0]);
    close(fds[1]);
    *out = c;
    return 0;
fail:;
    int saved = errno;
    for (int i = 0; i < 3; i++)
        if (fds[i] >= 0)
            close(fds[i]);
    dbw_close(c);
    errno = saved;
    return -1;
}

int dbw_start(dbw_client *c)
{
    return command(c, DBW_START);
}
int dbw_stop(dbw_client *c)
{
    return command(c, DBW_STOP);
}

void dbw_close(dbw_client *c)
{
    if (!c)
        return;
    if (c->ring)
        munmap((void *)c->ring, c->map_size);
    if (c->cursor)
        munmap(c->cursor, DBW_HEADER_SIZE);
    if (c->event_fd >= 0)
        close(c->event_fd);
    if (c->socket_fd >= 0)
        close(c->socket_fd);
    free(c);
}

ssize_t dbw_peek(dbw_client *c, dbw_span spans[2], size_t max_count)
{
    if (!c || !spans) {
        errno = EINVAL;
        return -1;
    }
    spans[0] = spans[1] = (dbw_span){0};
    if (!max_count)
        return 0;
    uint64_t head = atomic_load_explicit(&c->ring->head, memory_order_acquire);
    uint64_t available = head - c->tail;
    if (available > c->capacity) {
        errno = EPROTO;
        return -1;
    }
    size_t n = available < max_count ? (size_t)available : max_count;
    size_t pos = c->tail & (c->capacity - 1);
    size_t first = n < c->capacity - pos ? n : c->capacity - pos;
    spans[0] = (dbw_span){c->samples + pos, first};
    spans[1] = (dbw_span){c->samples, n - first};
    c->borrowed = n;
    return (ssize_t)n;
}

int dbw_consume(dbw_client *c, size_t n)
{
    if (!c || n > c->borrowed) {
        errno = EINVAL;
        return -1;
    }
    if (!n)
        return 0;
    c->tail += n;
    c->borrowed -= n;
    atomic_store_explicit(&c->cursor->tail, c->tail, memory_order_release);
    return 0;
}

ssize_t dbw_read_batch(dbw_client *c, dbw_sample *out, size_t max_count)
{
    if (!out && max_count) {
        errno = EINVAL;
        return -1;
    }
    dbw_span spans[2];
    ssize_t n = dbw_peek(c, spans, max_count);
    if (n <= 0)
        return n;
    memcpy(out, spans[0].samples, spans[0].count * sizeof(*out));
    if (spans[1].count)
        memcpy(out + spans[0].count, spans[1].samples, spans[1].count * sizeof(*out));
    if (dbw_consume(c, (size_t)n))
        return -1;
    return n;
}

int dbw_get_stats(dbw_client *c, dbw_stats *out)
{
    if (!c || !out) {
        errno = EINVAL;
        return -1;
    }
    uint64_t head = atomic_load_explicit(&c->ring->head, memory_order_acquire);
    *out = (dbw_stats){.produced = head,
                       .dropped = atomic_load_explicit(&c->ring->dropped, memory_order_relaxed),
                       .available = head - c->tail,
                       .interval_ns = c->ring->interval_ns,
                       .peak_bytes_per_sec = c->ring->peak_bytes_per_sec,
                       .capacity = c->capacity,
                       .state = atomic_load_explicit(&c->ring->state, memory_order_acquire)};
    return 0;
}

int dbw_wait(dbw_client *c, int timeout_ms)
{
    if (!c || timeout_ms < -1) {
        errno = EINVAL;
        return -1;
    }
    uint64_t deadline = timeout_ms < 0 ? 0 : dbw_now() + (uint64_t)timeout_ms * 1000000;
    for (;;) {
        atomic_store_explicit(&c->cursor->waiting, 1, memory_order_seq_cst);
        uint64_t head = atomic_load_explicit(&c->ring->head, memory_order_seq_cst);
        uint32_t state = atomic_load_explicit(&c->ring->state, memory_order_seq_cst);
        if (head != c->tail || state != DBW_RUNNING) {
            atomic_store_explicit(&c->cursor->waiting, 0, memory_order_seq_cst);
            if (head != c->tail)
                return 1;
            if (state == DBW_DEAD) {
                errno = EPIPE;
                return -1;
            }
            struct pollfd peer = {.fd = c->socket_fd, .events = POLLIN};
            if (poll(&peer, 1, 0) > 0 && peer.revents) {
                errno = EPIPE;
                return -1;
            }
            return 0;
        }
        int remaining = -1;
        if (timeout_ms >= 0) {
            uint64_t now = dbw_now();
            remaining = now >= deadline ? 0 : (int)((deadline - now + 999999) / 1000000);
        }
        struct pollfd fds[2] = {{.fd = c->event_fd, .events = POLLIN},
                                {.fd = c->socket_fd, .events = POLLIN}};
        int result = poll(fds, 2, remaining);
        int saved = errno;
        atomic_store_explicit(&c->cursor->waiting, 0, memory_order_seq_cst);
        if (result < 0) {
            if (saved == EINTR)
                continue;
            errno = saved;
            return -1;
        }
        if (!result)
            return 0;
        if (fds[0].revents & POLLIN) {
            uint64_t value;
            ssize_t ignored = recv(c->event_fd, &value, sizeof(value), MSG_DONTWAIT);
            (void)ignored;
        }
        if (fds[1].revents) {
            if (atomic_load_explicit(&c->ring->head, memory_order_acquire) != c->tail)
                return 1;
            errno = EPIPE;
            return -1;
        }
    }
}
