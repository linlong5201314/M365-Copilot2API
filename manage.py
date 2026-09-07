#!/usr/bin/env python3
"""M365 Copilot2API 管理脚本"""
import subprocess
import sys
import time
import os
import signal

# 基于脚本自身位置推导，克隆到任意目录都可直接用，无需改硬编码路径。
BASE_DIR = os.path.dirname(os.path.abspath(__file__))
SERVER_EXE = os.path.join(BASE_DIR, "m365-copilot2api.exe")
SERVER_DIR = BASE_DIR
LOG_FILE = os.path.join(BASE_DIR, "server.log")
ERR_FILE = os.path.join(BASE_DIR, "server-error.log")
PID_FILE = os.path.join(BASE_DIR, "server.pid")
DATA_DIR = os.path.join(BASE_DIR, "data")

def get_pid():
    try:
        with open(PID_FILE, 'r') as f:
            return int(f.read().strip())
    except:
        return None

def is_running(pid):
    try:
        os.kill(pid, 0)
        return True
    except:
        return False

def start():
    pid = get_pid()
    if pid and is_running(pid):
        print(f"Server already running (PID {pid})")
        return

    env = os.environ.copy()
    admin_pw = env.get("M365_ADMIN_PASSWORD", "admin123")
    env.update({
        "M365_LISTEN": env.get("M365_LISTEN", "0.0.0.0:4141"),
        "M365_DATA_DIR": os.path.join(DATA_DIR, ""),
        "M365_CONFIG": os.path.join(DATA_DIR, "accounts.json"),
        "M365_TOKEN_CACHE": os.path.join(DATA_DIR, "token-cache.json"),
        "M365_SESSION_CACHE": os.path.join(DATA_DIR, "sessions.json"),
        "M365_API_KEYS": os.path.join(DATA_DIR, "api-keys.json"),
        "M365_ADMIN_PASSWORD": admin_pw,
        "M365_CLEANUP_MODE": "keep_n",
        "M365_CLEANUP_KEEP_N": "3",
        "PATH": r"D:\go\bin;" + env.get("PATH", ""),
    })

    log = open(LOG_FILE, 'w')
    err = open(ERR_FILE, 'w')

    if sys.platform == 'win32':
        proc = subprocess.Popen(
            [SERVER_EXE],
            cwd=SERVER_DIR,
            env=env,
            stdout=log,
            stderr=err,
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.DETACHED_PROCESS
        )
    else:
        proc = subprocess.Popen(
            [SERVER_EXE],
            cwd=SERVER_DIR,
            env=env,
            stdout=log,
            stderr=err,
            start_new_session=True
        )

    with open(PID_FILE, 'w') as f:
        f.write(str(proc.pid))

    time.sleep(3)

    if is_running(proc.pid):
        print(f"Server started (PID {proc.pid})")
    else:
        print("Server failed to start!")
        print("--- Error log ---")
        with open(ERR_FILE, 'r') as f:
            print(f.read()[-2000:])

def stop():
    pid = get_pid()
    if not pid:
        print("No server running")
        return

    try:
        os.kill(pid, signal.SIGTERM)
        time.sleep(2)
        if is_running(pid):
            os.kill(pid, signal.SIGKILL)
        print(f"Server stopped (PID {pid})")
    except ProcessLookupError:
        print(f"Process {pid} already gone")
    except Exception as e:
        print(f"Error stopping: {e}")

    try:
        os.remove(PID_FILE)
    except:
        pass

def status():
    pid = get_pid()
    if pid and is_running(pid):
        print(f"Server running (PID {pid})")
    else:
        print("Server not running")

def logs(lines=20):
    try:
        with open(LOG_FILE, 'r') as f:
            all_lines = f.readlines()
            print(''.join(all_lines[-lines:]))
    except:
        print("No log file")

def err_logs(lines=20):
    try:
        with open(ERR_FILE, 'r') as f:
            all_lines = f.readlines()
            print(''.join(all_lines[-lines:]))
    except:
        print("No error log")

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: python manage.py <start|stop|status|logs|err>")
        sys.exit(1)

    cmd = sys.argv[1]
    if cmd == "start":
        start()
    elif cmd == "stop":
        stop()
    elif cmd == "status":
        status()
    elif cmd == "logs":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 20
        logs(n)
    elif cmd == "err":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 20
        err_logs(n)
    else:
        print(f"Unknown command: {cmd}")
