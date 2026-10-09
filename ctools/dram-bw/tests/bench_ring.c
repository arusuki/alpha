#define _GNU_SOURCE
#include "internal.h"
#include <assert.h>
#include <getopt.h>
#include <inttypes.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

#define RECORDS 2000000ull

static void consume(const char *path, size_t batch_size, bool zero_copy)
{
    dbw_client *c;
    assert(dbw_open(&c, path, 4096) == 0);
    assert(dbw_start(c) == 0);
    dbw_sample batch[256];
    uint64_t count = 0, checksum = 0, start = dbw_now();
    while (count < RECORDS) {
        dbw_span spans[2];
        ssize_t n;
        if (zero_copy)
            n = dbw_peek(c, spans, batch_size);
        else {
            n = dbw_read_batch(c, batch, batch_size);
            spans[0] = (dbw_span){batch, n > 0 ? (size_t)n : 0};
            spans[1] = (dbw_span){0};
        }
        assert(n >= 0);
        for (unsigned k = 0; k < 2; k++)
            for (size_t j = 0; j < spans[k].count; j++) {
                const dbw_sample *s = &spans[k].samples[j];
                assert(s->sequence == count + 1);
                assert(s->timestamp_ns == s->sequence * 7);
                assert(s->interval_ns == s->sequence * 3);
                assert(s->total_bytes_per_sec == (double)s->sequence);
                checksum += s->sequence;
                count++;
            }
        if (zero_copy)
            assert(dbw_consume(c, (size_t)n) == 0);
    }
    uint64_t elapsed = dbw_now() - start;
    assert(checksum == RECORDS * (RECORDS + 1) / 2);
    printf("%s,%zu,%" PRIu64 ",%.2f,%.2f\n", zero_copy ? "peek" : "copy", batch_size, count,
           (double)elapsed / (double)count, (double)count * 1e3 / (double)elapsed);
    fflush(stdout);
    dbw_close(c);
}

static void run(size_t batch, bool zero_copy)
{
    char directory[] = "/tmp/dbw-bench-XXXXXX";
    assert(mkdtemp(directory));
    struct sockaddr_un address = {.sun_family = AF_UNIX};
    snprintf(address.sun_path, sizeof(address.sun_path), "%s/socket", directory);
    int listener = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
    assert(listener >= 0);
    assert(bind(listener, (struct sockaddr *)&address, sizeof(address)) == 0);
    assert(listen(listener, 1) == 0);
    fflush(stdout);
    pid_t child = fork();
    assert(child >= 0);
    if (!child) {
        close(listener);
        consume(address.sun_path, batch, zero_copy);
        _exit(0);
    }
    int peer = accept4(listener, NULL, NULL, SOCK_CLOEXEC);
    assert(peer >= 0);
    struct dbw_msg msg;
    size_t nfds = 0;
    assert(dbw_recv_msg(peer, &msg, NULL, &nfds) == 0 && msg.op == DBW_OPEN);
    struct dbw_producer p;
    assert(dbw_producer_create(&p, msg.capacity, 1000, 0) == 0);
    int fds[] = {p.data_fd, p.cursor_fd, p.event_fd};
    assert(dbw_send_msg(peer, &msg, fds, 3) == 0);
    assert(dbw_recv_msg(peer, &msg, NULL, &nfds) == 0 && msg.op == DBW_START);
    dbw_producer_state(&p, DBW_RUNNING);
    assert(dbw_send_msg(peer, &msg, NULL, 0) == 0);
    for (uint64_t i = 1; i <= RECORDS; i++) {
        dbw_sample s = {.sequence = i,
                        .timestamp_ns = i * 7,
                        .interval_ns = i * 3,
                        .total_bytes_per_sec = (double)i};
        int result;
        do {
            result = dbw_producer_push(&p, &s);
            assert(result >= 0);
            if (!result)
                sched_yield();
        } while (!result);
    }
    int status;
    assert(waitpid(child, &status, 0) == child && WIFEXITED(status) && WEXITSTATUS(status) == 0);
    dbw_producer_destroy(&p);
    close(peer);
    close(listener);
    unlink(address.sun_path);
    rmdir(directory);
}

static void usage(FILE *out)
{
    fprintf(out, "Usage: bench-ring [OPTIONS]\n"
                 "\n"
                 "Benchmark shared-memory record transfer through the public client API.\n"
                 "Starts its own producer and consumer processes; no dram-bwd or PMU needed.\n"
                 "Tests copy and zero-copy peek with batches of 1, 64 and 256 records.\n"
                 "Each case transfers and validates 2000000 synthetic records.\n"
                 "Measures busy-polling IPC throughput, not hardware DRAM bandwidth.\n"
                 "\n"
                 "Options:\n"
                 "  -h, --help    Show this help and exit without running the benchmark.\n"
                 "\n"
                 "Output: CSV on stdout with columns:\n"
                 "  mode,batch,records,ns_per_record,million_records_per_sec\n"
                 "Build and run: make bench\n"
                 "Save CSV after building: ./build/bench-ring > ring.csv\n"
                 "Exit status: 0 = help or completed benchmark; 2 = usage error.\n"
                 "Failed validation assertions abort the process.\n");
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
    puts("mode,batch,records,ns_per_record,million_records_per_sec");
    for (unsigned mode = 0; mode < 2; mode++) {
        run(1, mode != 0);
        run(64, mode != 0);
        run(256, mode != 0);
    }
    return 0;
}
