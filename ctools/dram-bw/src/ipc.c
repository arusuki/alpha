#define _GNU_SOURCE
#include "internal.h"
#include <errno.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int dbw_send_msg(int fd, const struct dbw_msg *msg, const int *fds, size_t nfds)
{
    if (nfds > 3) {
        errno = EINVAL;
        return -1;
    }
    struct iovec iov = {.iov_base = (void *)msg, .iov_len = sizeof(*msg)};
    union {
        struct cmsghdr align;
        char buf[CMSG_SPACE(3 * sizeof(int))];
    } control = {0};
    struct msghdr hdr = {.msg_iov = &iov, .msg_iovlen = 1};
    if (nfds) {
        hdr.msg_control = control.buf;
        hdr.msg_controllen = CMSG_SPACE(nfds * sizeof(int));
        struct cmsghdr *c = CMSG_FIRSTHDR(&hdr);
        c->cmsg_level = SOL_SOCKET;
        c->cmsg_type = SCM_RIGHTS;
        c->cmsg_len = CMSG_LEN(nfds * sizeof(int));
        memcpy(CMSG_DATA(c), fds, nfds * sizeof(int));
    }
    ssize_t n;
    do {
        n = sendmsg(fd, &hdr, MSG_NOSIGNAL);
    } while (n < 0 && errno == EINTR);
    if (n == sizeof(*msg))
        return 0;
    if (n >= 0)
        errno = EIO;
    return -1;
}

int dbw_recv_msg(int fd, struct dbw_msg *msg, int *fds, size_t *nfds)
{
    size_t capacity = *nfds, used = 0;
    *nfds = 0;
    struct iovec iov = {.iov_base = msg, .iov_len = sizeof(*msg)};
    union {
        struct cmsghdr align;
        char buf[CMSG_SPACE(8 * sizeof(int))];
    } control;
    struct msghdr hdr = {.msg_iov = &iov,
                         .msg_iovlen = 1,
                         .msg_control = control.buf,
                         .msg_controllen = sizeof(control.buf)};
    ssize_t n;
    do {
        n = recvmsg(fd, &hdr, MSG_CMSG_CLOEXEC);
    } while (n < 0 && errno == EINTR);
    if (n < 0)
        return -1;
    bool bad = n != sizeof(*msg) || (hdr.msg_flags & (MSG_TRUNC | MSG_CTRUNC));
    for (struct cmsghdr *c = CMSG_FIRSTHDR(&hdr); c; c = CMSG_NXTHDR(&hdr, c)) {
        if (c->cmsg_level != SOL_SOCKET || c->cmsg_type != SCM_RIGHTS) {
            bad = true;
            continue;
        }
        size_t count = (c->cmsg_len - CMSG_LEN(0)) / sizeof(int);
        for (size_t i = 0; i < count; i++) {
            int received;
            memcpy(&received, CMSG_DATA(c) + i * sizeof(int), sizeof(int));
            if (used < capacity)
                fds[used++] = received;
            else {
                close(received);
                bad = true;
            }
        }
    }
    if (bad || msg->magic != DBW_MAGIC || msg->version != DBW_VERSION || msg->reserved) {
        for (size_t i = 0; i < used; i++) {
            close(fds[i]);
            fds[i] = -1;
        }
        errno = n == 0 ? EPIPE : EPROTO;
        return -1;
    }
    *nfds = used;
    return 0;
}
