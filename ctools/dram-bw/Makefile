CC ?= cc
AR ?= ar
CPPFLAGS += -Iinclude -Isrc
CFLAGS ?= -O3 -g
CFLAGS += -std=c11 -Wall -Wextra -Wpedantic -Werror -fPIC
LDLIBS += -lm
BUILD ?= build
PREFIX ?= /usr/local
RELEASE_DIR ?= $(BUILD)/release
RELEASE_NAME ?= dram-bw-linux-$(shell uname -m)

# Foreground hardware validation server; invoke with sudo after building.
PYTHON ?= python3
SERVER_DIR ?= /run/dram-bw
SERVER_BACKEND ?= amd-rome
SERVER_INTERVAL_US ?= 1000000
SERVER_UID ?= $(if $(SUDO_UID),$(SUDO_UID),$(shell id -u))
SERVER_GID ?= $(if $(SUDO_GID),$(SUDO_GID),$(shell id -g))
SERVER_REFERENCE ?= 0

LIBOBJ = $(BUILD)/client.o $(BUILD)/ipc.o
DAEMONOBJ = $(BUILD)/daemon.o $(BUILD)/backend.o $(BUILD)/ring.o $(BUILD)/ipc.o
DEPS = $(wildcard include/*.h src/*.h)

.PHONY: all clean test bench install release server help
all: $(BUILD)/libdram_bw.a $(BUILD)/libdram_bw.so $(BUILD)/dram-bwd $(BUILD)/dram-bw-consume

help:
	@printf '%s\n' \
	  'Usage: make [TARGET] [VARIABLE=value ...]' \
	  '' \
	  'Targets:' \
	  '  all (default)  Build dram-bwd, dram-bw-consume, libdram_bw.a and libdram_bw.so.' \
	  '  test           Run core/client tests with mock daemons and Python regressions.' \
	  '  bench          Build and run bench-ring (IPC throughput, not DRAM bandwidth).' \
	  '  server         Build and start the foreground daemon via scripts/server.py.' \
	  '  install        Install the two programs, both libraries and dram_bw.h.' \
	  '                 Does not install or start the systemd unit.' \
	  '  release        Package programs, server script, libraries and header as tar.gz.' \
	  '  clean          Remove the selected BUILD directory; keep measurement results.' \
	  '  help           Show this help without building or running anything.' \
	  '' \
	  'Build variables: BUILD=build CC=cc PREFIX=/usr/local DESTDIR= PYTHON=python3' \
	  'Release defaults: RELEASE_DIR=$(BUILD)/release RELEASE_NAME=dram-bw-linux-<arch>' \
	  'Server defaults: SERVER_BACKEND=amd-rome SERVER_INTERVAL_US=1000000' \
	  '                 SERVER_DIR=/run/dram-bw SERVER_REFERENCE=0' \
	  '                 SERVER_UID/GID use the sudo caller or current user.' \
	  '' \
	  'Example: make -j, then sudo make server; connect with:' \
	  '  ./build/dram-bw-consume /run/dram-bw/control.sock 10' \
	  'Every built executable supports -h and --help. See README.md for examples.'

$(BUILD):
	mkdir -p $@
$(BUILD)/%.o: src/%.c $(DEPS) | $(BUILD)
	$(CC) $(CPPFLAGS) $(CFLAGS) -c $< -o $@
$(BUILD)/libdram_bw.a: $(LIBOBJ)
	$(AR) rcs $@ $^
$(BUILD)/libdram_bw.so: $(LIBOBJ)
	$(CC) $(LDFLAGS) -shared -Wl,-soname,libdram_bw.so -o $@ $^ $(LDLIBS)
$(BUILD)/dram-bwd: $(DAEMONOBJ)
	$(CC) $(LDFLAGS) -o $@ $^ $(LDLIBS)
$(BUILD)/dram-bw-consume: examples/consume.c $(BUILD)/libdram_bw.a
	$(CC) $(CPPFLAGS) $(CFLAGS) $(LDFLAGS) -o $@ $^ $(LDLIBS)
$(BUILD)/test-core: tests/test_core.c $(BUILD)/ring.o $(BUILD)/ipc.o $(BUILD)/backend.o
	$(CC) $(CPPFLAGS) $(CFLAGS) $(LDFLAGS) -o $@ $^ $(LDLIBS)
$(BUILD)/test-client: tests/test_client.c $(BUILD)/libdram_bw.a
	$(CC) $(CPPFLAGS) $(CFLAGS) $(LDFLAGS) -o $@ $^ $(LDLIBS)
$(BUILD)/bench-ring: tests/bench_ring.c $(BUILD)/ring.o $(BUILD)/libdram_bw.a
	$(CC) $(CPPFLAGS) $(CFLAGS) $(LDFLAGS) -o $@ $^ $(LDLIBS)
test: all $(BUILD)/test-core $(BUILD)/test-client
	$(BUILD)/test-core
	$(PYTHON) tests/integration.py $(BUILD)
	$(PYTHON) -m unittest discover -s tests -p 'test_*.py'
bench: $(BUILD)/bench-ring
	$(BUILD)/bench-ring
server: all
	$(PYTHON) scripts/server.py --build "$(BUILD)" --directory "$(SERVER_DIR)" \
		--backend "$(SERVER_BACKEND)" --interval-us "$(SERVER_INTERVAL_US)" \
		--uid "$(SERVER_UID)" --gid "$(SERVER_GID)" --reference "$(SERVER_REFERENCE)"
install: all
	install -d "$(DESTDIR)$(PREFIX)/bin" "$(DESTDIR)$(PREFIX)/lib" "$(DESTDIR)$(PREFIX)/include"
	install -m 0755 $(BUILD)/dram-bwd $(BUILD)/dram-bw-consume "$(DESTDIR)$(PREFIX)/bin/"
	install -m 0755 $(BUILD)/libdram_bw.so "$(DESTDIR)$(PREFIX)/lib/"
	install -m 0644 $(BUILD)/libdram_bw.a "$(DESTDIR)$(PREFIX)/lib/"
	install -m 0644 include/dram_bw.h "$(DESTDIR)$(PREFIX)/include/"
release: all
	@mkdir -p "$(RELEASE_DIR)"
	@set -eu; \
	stage=$$(mktemp -d "$(RELEASE_DIR)/.dram-bw.XXXXXX"); \
	trap 'rm -rf "$$stage"' EXIT HUP INT TERM; \
	package="$$stage/$(RELEASE_NAME)"; \
	$(MAKE) --no-print-directory install DESTDIR="$$package" PREFIX=; \
	install -d "$$package/scripts" "$$package/systemd" "$$package/examples"; \
	install -m 0644 scripts/server.py "$$package/scripts/"; \
	install -m 0644 systemd/dram-bwd.service "$$package/systemd/"; \
	install -m 0644 examples/dram-bw.default "$$package/examples/"; \
	install -m 0644 docs/release.md "$$package/README.md"; \
	tar -czf "$$stage/package.tar.gz" -C "$$stage" "$(RELEASE_NAME)"; \
	mv "$$stage/package.tar.gz" "$(RELEASE_DIR)/$(RELEASE_NAME).tar.gz"; \
	printf 'Release: %s\n' "$(RELEASE_DIR)/$(RELEASE_NAME).tar.gz"
clean:
	rm -rf $(BUILD)
