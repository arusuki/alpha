#!/usr/bin/env python3
"""Upgrade the pre-cluster v14 development database to the current worker schema.

Run with services stopped. An SQLite backup is saved next to the database;
all changes commit together, and failures leave the original database intact.
"""
import argparse
import fcntl
import json
import secrets
import sqlite3
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def statements(path):
    pending = ''
    for line in (ROOT / path).read_text().splitlines(True):
        pending += line
        if sqlite3.complete_statement(pending):
            yield pending
            pending = ''
    if pending.strip():
        raise ValueError(f'incomplete SQL in {path}')


def upgrade(directory):
    path = directory / 'platform.sqlite3'
    if not path.is_file():
        raise ValueError('database does not exist')
    with (directory / 'service.lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError('开发服务仍在使用此数据目录；请先停止该服务再升级，数据库未修改') from None
        with sqlite3.connect(path, isolation_level=None) as db:
            version = db.execute('PRAGMA user_version').fetchone()[0]
            if version != 14:
                raise ValueError(f'expected v14, found v{version}; refusing to modify')
            backup_path = directory / f'platform.sqlite3.v14-backup-{time.time_ns()}'
            with sqlite3.connect(backup_path) as backup:
                db.backup(backup)
            backup_path.chmod(0o600)
            before = {name: db.execute(f'SELECT count(*) FROM "{name}"').fetchone()[0]
                      for name, in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name<>'sqlite_sequence'")}
            db.execute('PRAGMA foreign_keys=ON')
            db.execute('BEGIN IMMEDIATE')
            try:
                for sql in statements('internal/platform/schema.sql'):
                    if 'CREATE TABLE service_identity' in sql:
                        db.execute(sql)
                db.execute("INSERT INTO service_identity VALUES(1,'worker',?)", (secrets.token_hex(16),))
                for sql in statements('internal/containers/schema.sql'):
                    db.execute(sql)
                db.execute('INSERT INTO container_settings VALUES(1,?)', (json.dumps(dict(
                    endpoint='unix:///var/run/docker.sock', image='', base_dir='/docker', start_port=2222,
                    ssh_host='', proxy_jump='')),))
                db.execute("CREATE UNIQUE INDEX owners_one_container ON owners(owner) WHERE owner<>''")
                for sql in statements('internal/platform/container_ownership.sql'):
                    db.execute(sql)
                db.execute("ALTER TABLE agent_sessions ADD COLUMN node_id TEXT NOT NULL DEFAULT ''")
                db.execute('DROP INDEX idx_agent_sessions_user')
                db.execute('CREATE INDEX idx_agent_sessions_user ON agent_sessions(node_id,user_id,updated_at DESC)')
                db.execute('ALTER TABLE agent_cleanups RENAME TO upgrade_old_cleanups')
                db.execute('ALTER TABLE agent_cleanup_entries RENAME TO upgrade_old_entries')
                for sql in statements('internal/agent/schema.sql'):
                    if 'CREATE TABLE agent_cleanups ' in sql or 'CREATE TABLE agent_cleanup_entries ' in sql:
                        db.execute(sql)
                phases = list(db.execute('SELECT session_id,phase FROM upgrade_old_cleanups'))
                db.execute("""INSERT INTO agent_cleanups(id,report_id,status,error,created_at,updated_at)
                    SELECT c.session_id,c.report_id,'ready','',s.created_at,s.updated_at
                    FROM upgrade_old_cleanups c JOIN agent_sessions s ON s.id=c.session_id""")
                db.execute('''INSERT INTO agent_cleanup_entries
                    SELECT id,session_id,path,category,summary,detail,status,error FROM upgrade_old_entries''')
                db.execute('DROP TABLE upgrade_old_entries')
                db.execute('DROP TABLE upgrade_old_cleanups')
                for name, count in before.items():
                    actual = db.execute(f'SELECT count(*) FROM "{name}"').fetchone()[0]
                    if count != actual:
                        raise ValueError(f'row count changed: {name}: {count} -> {actual}')
                db.execute('INSERT INTO audit(at,actor,action,detail) VALUES(?,?,?,?)', (
                    time.time(), 'database-upgrade', 'database.upgrade',
                    json.dumps(dict(from_version=14, to_version=34, previous_cleanup_phases=phases))))
                if db.execute('PRAGMA foreign_key_check').fetchall():
                    raise ValueError('foreign key check failed')
                if db.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
                    raise ValueError('integrity check failed')
                db.execute('PRAGMA user_version=34')
                db.execute('COMMIT')
            except Exception:
                db.execute('ROLLBACK')
                raise
            print(json.dumps(dict(database=str(path), version=34, backup=str(backup_path), retained_rows=before), ensure_ascii=False))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('data_dir', type=Path)
    args = parser.parse_args()
    upgrade(args.data_dir.resolve())
