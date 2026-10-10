#define _GNU_SOURCE
#include "internal.h"
#include "backend.h"
#include <errno.h>
#include <getopt.h>
#include <fcntl.h>
#include <limits.h>
#include <math.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/signalfd.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/timerfd.h>
#include <sys/un.h>
#include <unistd.h>

struct peer {
    int fd;
    bool opened, running;
    uint64_t connected_ns, start_ns;
    struct dbw_producer producer;
};

struct server {
    struct peer peers[DBW_MAX_CLIENTS];
    struct dbw_backend backend;
    unsigned active;
    int timer_fd;
    uint64_t interval_ns;
};

static int timer_set(struct server *s, bool enabled)
{
    struct itimerspec spec = {0};
    if (enabled) {
        /* One shot: start the next delay only AFTER reading, logging and
         * publishing this round. Never consume a backlog of periodic ticks. */
        spec.it_value.tv_sec = (time_t)(s->interval_ns / 1000000000);
        spec.it_value.tv_nsec = (long)(s->interval_ns % 1000000000);
    }
    return timerfd_settime(s->timer_fd, 0, &spec, NULL);
}

static void stop_peer(struct server *s, struct peer *p)
{
    if (p->running) {
        p->running = false;
        if (!--s->active) {
            timer_set(s, false);
            dbw_backend_stop(&s->backend);
        }
        dbw_producer_state(&p->producer, DBW_STOPPED);
    }
}

static void close_peer(struct server *s, struct peer *p)
{
    stop_peer(s, p);
    if (p->opened)
        dbw_producer_destroy(&p->producer);
    close(p->fd);
    memset(p, 0, sizeof(*p));
    p->fd = -1;
}

static void handle_peer(struct server *s, struct peer *p)
{
    struct dbw_msg msg;
    size_t nfds = 0;
    if (dbw_recv_msg(p->fd, &msg, NULL, &nfds)) {
        if (errno != EAGAIN && errno != EWOULDBLOCK)
            close_peer(s, p);
        return;
    }
    if (msg.error || (msg.op != DBW_OPEN && msg.capacity)) {
        close_peer(s, p);
        return;
    }
    int fds[3];
    if (msg.op == DBW_OPEN) {
        if (p->opened)
            msg.error = EALREADY;
        else if (!dbw_capacity_valid(msg.capacity))
            msg.error = EINVAL;
        else if (dbw_producer_create(&p->producer, msg.capacity, s->interval_ns, s->backend.peak))
            msg.error = errno;
        else {
            p->opened = true;
            fds[0] = p->producer.data_fd;
            fds[1] = p->producer.cursor_fd;
            fds[2] = p->producer.event_fd;
            nfds = 3;
        }
    } else if (!p->opened)
        msg.error = ENOTCONN;
    else if (msg.op == DBW_START) {
        if (!p->running) {
            if (!s->active && dbw_backend_start(&s->backend))
                msg.error = errno;
            else if (!s->active && timer_set(s, true)) {
                msg.error = errno;
                dbw_backend_stop(&s->backend);
            } else {
                /* New subscribers skip a partially elapsed global interval. */
                p->start_ns = s->active ? dbw_now() : s->backend.previous_ns;
                p->running = true;
                s->active++;
                dbw_producer_state(&p->producer, DBW_RUNNING);
            }
        }
    } else if (msg.op == DBW_STOP)
        stop_peer(s, p);
    else
        msg.error = EINVAL;
    /* Nonblocking bounded response: an unresponsive peer cannot stall sampling. */
    if (dbw_send_msg(p->fd, &msg, fds, nfds))
        close_peer(s, p);
}

static int sample_all(struct server *s)
{
    uint64_t ticks;
    ssize_t n = read(s->timer_fd, &ticks, sizeof(ticks));
    if (n < 0 && (errno == EAGAIN || errno == EINTR))
        return 0;
    if (n != sizeof(ticks)) {
        if (n >= 0)
            errno = EIO;
        return -1;
    }
    if (!s->active)
        return 0;
    dbw_sample sample;
    dbw_backend_sample(&s->backend, &sample, s->interval_ns);
    for (unsigned i = 0; i < DBW_MAX_CLIENTS; i++) {
        struct peer *p = &s->peers[i];
        if (!p->running || sample.timestamp_ns - sample.interval_ns < p->start_ns)
            continue;
        if (dbw_producer_push(&p->producer, &sample) < 0)
            close_peer(s, p);
    }
    return s->active ? timer_set(s, true) : 0;
}

static void usage(FILE *out)
{
    fprintf(
        out,
        "Usage: dram-bwd [OPTIONS]\n"
        "\n"
        "DRAM bandwidth daemon: sample host PMU counters and serve clients.\n"
        "Runs in the foreground; Ctrl-C stops it. Use systemd for a service.\n"
        "\n"
        "Options:\n"
        "  -h, --help              Show this help and exit without opening hardware.\n"
        "  --socket PATH           Unix socket (default: /run/dram-bw/control.sock).\n"
        "                          Parent directory must exist; path must be unused.\n"
        "  --backend NAME          auto (default), amd-rome, or mock.\n"
        "                          auto detects AMD Family 17h / Model 31h (Rome).\n"
        "                          mock generates 1 GB/s read + 0.5 GB/s write.\n"
        "  --events FILE           Custom PMU events; use with --backend auto.\n"
        "                          Format: PMU CPU KIND CONFIG CONFIG1 CONFIG2 BYTES_PER_COUNT.\n"
        "  --interval-us N         Delay after each scan in microseconds.\n"
        "                          Default: 100000; range: 100..10000000.\n"
        "  --settings FILE         Optional persisted backend, interval and peak settings.\n"
        "                          Overrides CLI values; missing file uses CLI defaults.\n"
        "  --peak-gbps N           Positive reference peak in decimal GB/s.\n"
        "                          Optional; omitted => utilization is NaN.\n"
        "  --socket-mode OCTAL     Socket permissions (default: 0660).\n"
        "  --allow-uid HOST_UID    Restrict connections to this host-visible UID.\n"
        "                          Default: access controlled by socket permissions.\n"
        "  --diagnostics FILE      Append per-counter and sample JSONL diagnostics.\n"
        "                          Optional; adds file I/O to each scan.\n"
        "\n"
        "Hardware sampling needs perf permissions; mock needs no PMU access.\n"
        "There is no automatic mock fallback. Samples cover host memory traffic,\n"
        "not individual containers. Rome reports total bandwidth only.\n"
        "Sampling starts with the first subscription and stops with the last.\n"
        "Logs go to stderr; use dram-bw-consume to print samples as CSV.\n"
        "\n"
        "Example (create /tmp/dram-bw-demo first):\n"
        "  dram-bwd --backend mock --socket /tmp/dram-bw-demo/control.sock --peak-gbps 3\n"
        "  dram-bw-consume /tmp/dram-bw-demo/control.sock 10\n"
        "\n"
        "Exit status: 0 = help or normal shutdown; 1 = runtime failure; 2 = usage error.\n");
}

static int number(const char *str, unsigned long long *out, int base)
{
    char *end;
    errno = 0;
    *out = strtoull(str, &end, base);
    return errno || *str < '0' || *str > '9' || *end ? -1 : 0;
}

int main(int argc, char **argv)
{
    const char *path = "/run/dram-bw/control.sock", *backend = "auto", *events = NULL;
    const char *diagnostics = NULL, *settings = NULL;
    char settings_backend[16];
    unsigned long long interval = 100000, mode = 0660, uid = 0;
    bool restrict_uid = false;
    double peak = 0;
    const struct option options[] = {{"socket", required_argument, NULL, 's'},
                                     {"backend", required_argument, NULL, 'b'},
                                     {"events", required_argument, NULL, 'e'},
                                     {"interval-us", required_argument, NULL, 'i'},
                                     {"settings", required_argument, NULL, 'f'},
                                     {"peak-gbps", required_argument, NULL, 'p'},
                                     {"socket-mode", required_argument, NULL, 'm'},
                                     {"allow-uid", required_argument, NULL, 'u'},
                                     {"diagnostics", required_argument, NULL, 'd'},
                                     {"help", no_argument, NULL, 'h'},
                                     {0}};
    int opt;
    while ((opt = getopt_long(argc, argv, "h", options, NULL)) != -1) {
        switch (opt) {
        case 's':
            path = optarg;
            break;
        case 'b':
            backend = optarg;
            break;
        case 'e':
            events = optarg;
            break;
        case 'd':
            diagnostics = optarg;
            break;
        case 'f':
            settings = optarg;
            break;
        case 'i':
            if (number(optarg, &interval, 10))
                goto badargs;
            break;
        case 'm':
            if (number(optarg, &mode, 8))
                goto badargs;
            break;
        case 'u':
            if (number(optarg, &uid, 10))
                goto badargs;
            restrict_uid = true;
            break;
        case 'p': {
            char *end;
            errno = 0;
            peak = strtod(optarg, &end) * 1e9;
            if (errno || end == optarg || *end || !isfinite(peak) || peak <= 0)
                goto badargs;
            break;
        }
        case 'h':
            usage(stdout);
            return 0;
        default:
            goto badargs;
        }
    }
    if (settings) {
        int fd = open(settings, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK);
        if (fd < 0 && errno != ENOENT) {
            perror("dram-bwd settings");
            return 1;
        }
        if (fd >= 0) {
            struct stat st;
            char line[160], interval_text[32], peak_text[64], extra;
            FILE *file = fdopen(fd, "r");
            if (!file) { close(fd); return 1; }
            bool valid = !fstat(fd, &st) && S_ISREG(st.st_mode) &&
                fgets(line, sizeof(line), file) && fgetc(file) == EOF && !ferror(file) &&
                sscanf(line, "%15s %31s %63s %c", settings_backend, interval_text, peak_text, &extra) == 3;
            fclose(file);
            char *end = NULL;
            errno = 0;
            double value = valid ? strtod(peak_text, &end) : 0;
            valid = valid && end != peak_text && !*end && !errno && isfinite(value) && value >= 0 && value <= 1000000;
            valid = valid && !number(interval_text, &interval, 10) && interval >= 100 && interval <= 10000000;
            valid = valid && (!strcmp(settings_backend, "auto") || !strcmp(settings_backend, "amd-rome") || !strcmp(settings_backend, "mock"));
            if (!valid) {
                fprintf(stderr, "Invalid DRAM settings in %s; file preserved.\n", settings);
                return 2;
            }
            backend = settings_backend;
            peak = value * 1e9;
            /* A selected backend replaces custom event definitions as well. */
            events = NULL;
        }
    }
    if (optind != argc || interval < 100 || interval > 10000000 || mode > 0777 || uid > UINT_MAX ||
        !*path)
        goto badargs;
    struct sockaddr_un address = {.sun_family = AF_UNIX};
    if (strlen(path) >= sizeof(address.sun_path))
        goto badargs;
    strcpy(address.sun_path, path);
    struct server *s = calloc(1, sizeof(*s));
    if (!s) {
        perror("calloc");
        return 1;
    }
    for (unsigned i = 0; i < DBW_MAX_CLIENTS; i++)
        s->peers[i].fd = -1;
    s->timer_fd = -1;
    s->interval_ns = interval * 1000;
    int listen_fd = -1, signal_fd = -1, result = 1;
    bool bound = false;
    struct stat owned = {0};
    sigset_t mask;
    sigemptyset(&mask);
    sigaddset(&mask, SIGTERM);
    sigaddset(&mask, SIGINT);
    if (sigprocmask(SIG_BLOCK, &mask, NULL))
        goto fail;
    signal_fd = signalfd(-1, &mask, SFD_CLOEXEC | SFD_NONBLOCK);
    if (signal_fd < 0)
        goto fail;
    if (dbw_backend_open(&s->backend, backend, events, peak)) {
        fprintf(stderr, "DRAM backend unavailable: %s. No mock fallback.\n", strerror(errno));
        goto cleanup;
    }
    if (diagnostics && dbw_backend_diagnostics_open(&s->backend, diagnostics))
        goto fail;
    s->timer_fd = timerfd_create(CLOCK_MONOTONIC, TFD_CLOEXEC | TFD_NONBLOCK);
    listen_fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC | SOCK_NONBLOCK, 0);
    if (s->timer_fd < 0 || listen_fd < 0)
        goto fail;
    /* Never unlink someone else's live or stale socket on startup. */
    mode_t old_umask = umask(0777);
    int rc = bind(listen_fd, (struct sockaddr *)&address, sizeof(address));
    umask(old_umask);
    if (rc)
        goto fail;
    bound = true;
    if (lstat(path, &owned) || chmod(path, (mode_t)mode) || listen(listen_fd, 64))
        goto fail;
    fprintf(stderr, "dram-bwd: %s backend=%s counters=%zu delay=%llu us\n", path,
            s->backend.mock ? "MOCK" : (s->backend.total_only ? "perf-total" : "perf-read-write"),
            s->backend.count, interval);
    if (peak > 0)
        fprintf(stderr, "utilization reference peak: %.3f GB/s\n", peak / 1e9);
    else
        fprintf(stderr, "reporting bandwidth; utilization unavailable (no --peak-gbps)\n");
    if (diagnostics)
        fprintf(stderr, "diagnostics: %s (appending raw snapshots; extra I/O enabled)\n",
                diagnostics);
    if (!events && !s->backend.mock && interval < 100000)
        fprintf(
            stderr,
            "DF events multiplex: short intervals may have INVALID samples; prefer >=100 ms.\n");
    for (;;) {
        struct pollfd fds[3 + DBW_MAX_CLIENTS] = {{.fd = signal_fd, .events = POLLIN},
                                                  {.fd = s->timer_fd, .events = POLLIN},
                                                  {.fd = listen_fd, .events = POLLIN}};
        for (unsigned i = 0; i < DBW_MAX_CLIENTS; i++)
            fds[3 + i] = (struct pollfd){.fd = s->peers[i].fd, .events = POLLIN};
        rc = poll(fds, 3 + DBW_MAX_CLIENTS, 1000);
        if (rc < 0) {
            if (errno == EINTR)
                continue;
            goto fail;
        }
        if (fds[0].revents) {
            result = 0;
            break;
        }
        if (fds[1].revents && sample_all(s))
            goto fail;
        uint64_t now = dbw_now();
        for (unsigned i = 0; i < DBW_MAX_CLIENTS; i++) {
            struct peer *p = &s->peers[i];
            if (p->fd < 0)
                continue;
            short revents = fds[3 + i].revents;
            if ((revents & (POLLHUP | POLLERR | POLLNVAL)) ||
                (!p->opened && now - p->connected_ns >= 5000000000ull))
                close_peer(s, p);
            else if (revents & POLLIN)
                handle_peer(s, p);
        }
        if (fds[2].revents & POLLIN) {
            /* Bound accepts per iteration to preserve timer fairness. */
            for (unsigned n = 0; n < 8; n++) {
                int fd = accept4(listen_fd, NULL, NULL, SOCK_CLOEXEC | SOCK_NONBLOCK);
                if (fd < 0)
                    break;
                struct ucred cred;
                socklen_t len = sizeof(cred);
                if (getsockopt(fd, SOL_SOCKET, SO_PEERCRED, &cred, &len) ||
                    (restrict_uid && cred.uid != (uid_t)uid)) {
                    close(fd);
                    continue;
                }
                unsigned i;
                for (i = 0; i < DBW_MAX_CLIENTS && s->peers[i].fd >= 0; i++) {
                }
                if (i == DBW_MAX_CLIENTS)
                    close(fd);
                else
                    s->peers[i] = (struct peer){.fd = fd, .connected_ns = dbw_now()};
            }
        }
    }
    goto cleanup;
fail:
    perror("dram-bwd");
cleanup:
    for (unsigned i = 0; i < DBW_MAX_CLIENTS; i++)
        if (s->peers[i].fd >= 0)
            close_peer(s, &s->peers[i]);
    dbw_backend_close(&s->backend);
    if (listen_fd >= 0)
        close(listen_fd);
    if (signal_fd >= 0)
        close(signal_fd);
    if (s->timer_fd >= 0)
        close(s->timer_fd);
    struct stat current;
    if (bound && !lstat(path, &current) && current.st_dev == owned.st_dev &&
        current.st_ino == owned.st_ino)
        unlink(path);
    free(s);
    return result;
badargs:
    usage(stderr);
    return 2;
}
