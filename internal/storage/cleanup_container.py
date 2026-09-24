"""Embedded docker-exec worker. Requests are JSON on stdin; no shell or files."""
import ctypes
import json
import os
import queue
import re
import stat
import sys
import threading


class OpenHow(ctypes.Structure):
    _fields_ = [('flags', ctypes.c_uint64), ('mode', ctypes.c_uint64),
                ('resolve', ctypes.c_uint64)]


_libc = ctypes.CDLL(None, use_errno=True)
_libc.syscall.restype = ctypes.c_long


def open_dir(parent, path):
    # Linux openat2 is 437 on supported x86_64/aarch64 hosts. Fail closed on
    # other architectures; never substitute a symlink-following open().
    if os.uname().machine not in ('x86_64', 'aarch64'):
        raise RuntimeError('unsupported openat2 architecture')
    how = OpenHow(os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC, 0, 0x01 | 0x02 | 0x04 | 0x08)
    fd = _libc.syscall(ctypes.c_long(437), ctypes.c_int(parent),
                       ctypes.c_char_p(os.fsencode(path)), ctypes.byref(how), ctypes.sizeof(how))
    if fd < 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code), path)
    return fd


def within(path, root):
    return path == root or path.startswith(root.rstrip('/') + '/')


def mounts():
    unescape = lambda value: re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), value)
    result = []
    with open('/proc/self/mountinfo') as source:
        for line in source:
            left, right = line.rstrip('\n').split(' - ', 1)
            fields, fs = left.split(), right.split()
            options = {}
            for option in (fields[5] + ',' + fs[2]).split(','):
                key, _, value = option.partition('=')
                options[key] = unescape(value)
            result.append((unescape(fields[4]), fs[0], options))
    return result


def same_upper(actual, expected):
    if actual == expected:
        return True
    # Docker overlay2 can mount after chdir into its storage directory; Linux
    # then exposes "<layer-id>/diff" rather than an absolute upperdir. Accept
    # only that exact form and the same full layer ID validated by inspect.
    return (isinstance(actual, str) and re.fullmatch(r'[0-9a-f]{64}/diff', actual) is not None
            and os.path.basename(os.path.dirname(os.path.dirname(expected))) == 'overlay2'
            and expected.endswith('/' + actual))


def prepare(request):
    if set(request) != {'upper', 'paths', 'protected'}:
        raise ValueError('invalid cleanup request')
    paths, protected = request['paths'], request['protected']
    if not isinstance(paths, list) or not 1 <= len(paths) <= 100 or not isinstance(protected, list):
        raise ValueError('invalid cleanup paths')
    table = mounts()
    roots = [(fs, opts) for path, fs, opts in table if path == '/']
    if len(roots) != 1 or roots[0][0] != 'overlay' or not same_upper(roots[0][1].get('upperdir'), request['upper']):
        raise RuntimeError('container root mount or writable layer changed')
    if 'ro' in roots[0][1]:
        raise RuntimeError('container root mount is read-only')
    blocked = ['/proc', '/sys', '/dev', '/boot', '/etc', '/bin', '/sbin', '/lib', '/lib64', '/usr']
    blocked += protected + [path for path, _, _ in table if path != '/']
    for path in paths:
        if not isinstance(path, str) or not path.startswith('/') or path.startswith('//') or '\x00' in path or os.path.normpath(path) != path:
            raise ValueError('invalid container path')
        if path == '/' or os.path.dirname(path) == '/' and path != '/tmp':
            raise ValueError('protected container root: ' + path)
        if any(within(path, root) or within(root, path) for root in blocked):
            raise ValueError('protected path or mount: ' + path)
    for i, path in enumerate(paths):
        if any(within(path, other) or within(other, path) for other in paths[:i]):
            raise ValueError('overlapping cleanup paths')
    root = os.open('/', os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    targets = []
    try:
        for path in paths:
            try:
                parent = open_dir(root, os.path.dirname(path).lstrip('/') or '.')
            except FileNotFoundError:
                targets.append((None, os.path.basename(path), path))
                continue
            targets.append((parent, os.path.basename(path), path))
            try:
                info = os.stat(os.path.basename(path), dir_fd=parent, follow_symlinks=False)
            except FileNotFoundError:
                continue
            if stat.S_ISLNK(info.st_mode):
                raise ValueError('selected path became a symlink: ' + path)
            if stat.S_ISDIR(info.st_mode):
                fd = open_dir(parent, os.path.basename(path))
                os.close(fd)
        return targets
    except BaseException:
        for parent, _, _ in targets:
            if parent is not None:
                os.close(parent)
        raise
    finally:
        os.close(root)


def clean(parent, name, path, keep, counts, missing_ok=False):
    before_skipped = sum(counts.values())
    try:
        try:
            before = os.stat(name, dir_fd=parent, follow_symlinks=False)
        except FileNotFoundError:
            if missing_ok:
                return
            raise
        mode = before.st_mode
        if keep and stat.S_ISLNK(mode):
            raise RuntimeError('selected path became a symlink: ' + path)
        if stat.S_ISSOCK(mode):
            counts['skipped_sockets'] += 1
            return
        if stat.S_ISCHR(mode):
            counts['skipped_char_devices'] += 1
            return
        if not stat.S_ISDIR(mode):
            if not (stat.S_ISREG(mode) or stat.S_ISLNK(mode) or stat.S_ISFIFO(mode)):
                raise RuntimeError('unsupported file type: ' + path)
            os.unlink(name, dir_fd=parent)
            return
        fd = open_dir(parent, name)
        try:
            opened = os.fstat(fd)
            if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
                raise RuntimeError('directory changed while opening: ' + path)
            with os.scandir(fd) as children:
                for child in children:
                    clean(fd, child.name, path.rstrip('/') + '/' + child.name, False, counts)
            current = os.stat(name, dir_fd=parent, follow_symlinks=False)
            if (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino):
                raise RuntimeError('directory replaced during cleanup: ' + path)
            if not keep and sum(counts.values()) == before_skipped:
                os.rmdir(name, dir_fd=parent)
        finally:
            os.close(fd)
    except OSError as error:
        raise RuntimeError(path + ': ' + str(error)) from error


def emit(event):
    print(json.dumps(event, ensure_ascii=True), flush=True)


def main():
    targets = []
    try:
        raw = sys.stdin.buffer.readline(2 * 1024 * 1024)
        if not raw.endswith(b'\n'):
            raise ValueError('missing cleanup request')
        targets = prepare(json.loads(raw))
        commands = queue.Queue(maxsize=1)

        def control():
            while True:
                raw = sys.stdin.buffer.readline(4096)
                if not raw or not raw.endswith(b'\n'):
                    # Docker CLI cancellation/disconnect must stop deletion,
                    # including when the main thread is walking a large tree.
                    os._exit(0)
                commands.put(raw)

        threading.Thread(target=control, daemon=True).start()
        emit({'type': 'prepared'})
        completed = set()
        while True:
            request = json.loads(commands.get())
            index = request.get('index')
            if set(request) != {'index'} or type(index) is not int or not 0 <= index < len(targets) or index in completed:
                raise ValueError('invalid cleanup command')
            completed.add(index)
            parent, name, path = targets[index]
            counts = {'skipped_sockets': 0, 'skipped_char_devices': 0}
            event = dict(type='result', index=index)
            try:
                if parent is not None:
                    clean(parent, name, path, True, counts, missing_ok=True)
            except Exception as error:
                event['error'] = str(error)
            event.update(counts)
            emit(event)
    except Exception as error:
        emit({'type': 'error', 'error': str(error)})
        sys.exit(1)
    finally:
        for parent, _, _ in targets:
            if parent is not None:
                os.close(parent)


if __name__ == '__main__':
    main()
