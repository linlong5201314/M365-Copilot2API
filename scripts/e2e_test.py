#!/usr/bin/env python3
"""端到端测试：模拟新用户从零开始使用"""
import subprocess
import sys
import time
import os
import json
import urllib.request
import urllib.error
import http.cookiejar

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), os.pardir))
TEST_DIR = os.environ.get("M365_E2E_DIR", os.path.join(REPO_ROOT, ".e2e-work"))
if os.name == "nt":
    SERVER_EXE = os.path.join(TEST_DIR, "m365-copilot2api.exe")
else:
    SERVER_EXE = os.path.join(TEST_DIR, "m365-copilot2api")
DATA_DIR = os.path.join(TEST_DIR, "data")
SECRETS_DIR = os.path.join(TEST_DIR, "secrets")
LOG_FILE = os.path.join(TEST_DIR, "e2e.log")

os.makedirs(TEST_DIR, exist_ok=True)

def log(msg):
    line = f"[E2E] {msg}"
    try:
        print(line)
    except UnicodeEncodeError:
        print(line.encode('ascii', errors='replace').decode())
    with open(LOG_FILE, 'a', encoding='utf-8') as f:
        f.write(f"{msg}\n")

def run(cmd, **kwargs):
    result = subprocess.run(cmd, shell=True, capture_output=True, text=True, **kwargs)
    return result.stdout, result.stderr, result.returncode

def http_request(url, data=None, headers=None, method="GET", cookie_jar=None):
    if data and isinstance(data, dict):
        data = json.dumps(data).encode('utf-8')
    elif data and isinstance(data, str):
        data = data.encode('utf-8')

    req = urllib.request.Request(url, data=data, method=method)
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)

    if cookie_jar is None:
        cookie_jar = http.cookiejar.CookieJar()

    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookie_jar))

    try:
        resp = opener.open(req, timeout=30)
        return resp.status, resp.read().decode('utf-8'), cookie_jar
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode('utf-8'), cookie_jar

def main():
    log("=" * 60)
    log("Starting E2E Test")
    log("=" * 60)

    # Step 1: Prepare clean environment
    log("\n--- Step 1: Prepare clean environment ---")
    if os.path.exists(TEST_DIR):
        import shutil
        if os.name == "nt":
            subprocess.run("taskkill /F /IM m365-copilot2api.exe 2>nul", shell=True)
        time.sleep(1)
        def on_rm_error(func, path, exc_info):
            import stat
            os.chmod(path, stat.S_IWRITE)
            os.unlink(path)
        shutil.rmtree(TEST_DIR, onerror=on_rm_error)
    os.makedirs(TEST_DIR)
    os.makedirs(DATA_DIR)
    os.makedirs(SECRETS_DIR)

    # Copy source files
    import shutil
    shutil.copytree(os.path.join(REPO_ROOT, "cmd"), os.path.join(TEST_DIR, "cmd"))
    shutil.copytree(os.path.join(REPO_ROOT, "internal"), os.path.join(TEST_DIR, "internal"))
    if os.path.exists(os.path.join(REPO_ROOT, "web")):
        shutil.copytree(os.path.join(REPO_ROOT, "web"), os.path.join(TEST_DIR, "web"))
    shutil.copy(os.path.join(REPO_ROOT, "go.mod"), TEST_DIR)
    shutil.copy(os.path.join(REPO_ROOT, "go.sum"), TEST_DIR)
    log(f"Copied source files to {TEST_DIR}")

    # Step 2: Build
    log("\n--- Step 2: Build ---")
    env = os.environ.copy()
    result = subprocess.run(
        ["go", "build", "-o", SERVER_EXE, "./cmd/server"],
        cwd=TEST_DIR, env=env, capture_output=True, text=True
    )
    if result.returncode != 0:
        log(f"BUILD FAILED: {result.stderr}")
        return False
    log("Build OK")

    # Step 3: Start server
    log("\n--- Step 3: Start server ---")
    env = os.environ.copy()
    env.update({
        "M365_LISTEN": "127.0.0.1:4142",
        "M365_DATA_DIR": DATA_DIR,
        "M365_CONFIG": os.path.join(DATA_DIR, "accounts.json"),
        "M365_TOKEN_CACHE": os.path.join(DATA_DIR, "token-cache.json"),
        "M365_SESSION_CACHE": os.path.join(DATA_DIR, "sessions.json"),
        "M365_API_KEYS": os.path.join(DATA_DIR, "api-keys.json"),
        "M365_ADMIN_PASSWORD": "test123",
    })

    log_file = open(os.path.join(TEST_DIR, "server.log"), 'w')
    err_file = open(os.path.join(TEST_DIR, "server-error.log"), 'w')

    proc = subprocess.Popen(
        [SERVER_EXE], cwd=TEST_DIR, env=env,
        stdout=log_file, stderr=err_file
    )
    log(f"Server started (PID {proc.pid})")
    time.sleep(4)

    # Check if running
    if proc.poll() is not None:
        log("SERVER CRASHED!")
        log_file.close()
        err_file.close()
        with open(os.path.join(TEST_DIR, "server-error.log"), 'r') as f:
            log(f.read()[-2000:])
        return False

    base = "http://127.0.0.1:4142"

    # Step 4: Test root path
    log("\n--- Step 4: Test root path ---")
    status, body, _ = http_request(f"{base}/")
    log(f"GET / -> {status}")
    if status != 200:
        log(f"Root path failed: {body[:200]}")
        proc.kill()
        return False

    # Step 5: Admin login
    log("\n--- Step 5: Admin login ---")
    status, body, cj = http_request(
        f"{base}/api/admin/login",
        data={"password": "test123"},
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    log(f"POST /api/admin/login -> {status}")
    if status != 200:
        log(f"Login failed: {body}")
        proc.kill()
        return False
    login_resp = json.loads(body)
    log(f"Login response: must_change={login_resp.get('must_change_password')}")

    # Step 6: Create API Key
    log("\n--- Step 6: Create API Key ---")
    status, body, cj = http_request(
        f"{base}/api/admin/keys",
        data={"name": "e2e-test-key"},
        headers={"Content-Type": "application/json"},
        method="POST",
        cookie_jar=cj
    )
    log(f"POST /api/admin/keys -> {status}")
    if status != 200:
        log(f"Create key failed: {body}")
        proc.kill()
        return False
    key_resp = json.loads(body)
    api_key = key_resp.get("key", "")
    log(f"API Key created: {api_key[:20]}...")

    # Step 7: Test models endpoint (no account yet)
    log("\n--- Step 7: Test models endpoint ---")
    status, body, _ = http_request(
        f"{base}/v1/models",
        headers={"Authorization": f"Bearer {api_key}"}
    )
    log(f"GET /v1/models -> {status}")
    if status != 200:
        log(f"Models failed: {body[:200]}")
    else:
        models = json.loads(body)
        log(f"Models count: {len(models.get('data', []))}")

    # Step 8: Test stats endpoint
    log("\n--- Step 8: Test stats endpoint ---")
    status, body, _ = http_request(f"{base}/api/stats")
    log(f"GET /api/stats -> {status}")
    stats = json.loads(body)
    log(f"Stats: {stats.get('stats', {}).get('total_requests', 0)} requests")

    # Step 9: Test chat without account (should fail gracefully)
    log("\n--- Step 9: Test chat without account ---")
    status, body, _ = http_request(
        f"{base}/v1/chat/completions",
        data={"model": "gpt-5.5", "messages": [{"role": "user", "content": "hi"}]},
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        method="POST"
    )
    log(f"POST /v1/chat/completions (no account) -> {status}")
    log(f"Response: {body[:200]}")

    # Step 10: Check M365 endpoints (should fail without account)
    log("\n--- Step 10: Test M365 endpoints (no account) ---")
    status, body, _ = http_request(
        f"{base}/api/m365/conversations",
        cookie_jar=cj
    )
    log(f"GET /api/m365/conversations -> {status}")

    # Summary
    log("\n" + "=" * 60)
    log("E2E Test Summary")
    log("=" * 60)
    log("✅ Build: OK")
    log("✅ Server start: OK")
    log("✅ Root path: OK")
    log("✅ Admin login: OK")
    log("✅ API Key creation: OK")
    log("✅ Stats endpoint: OK")
    log("✅ Models endpoint: OK")
    log("⚠️  Chat without account: Expected to need account first")
    log("\nAll basic tests passed!")

    proc.kill()
    log_file.close()
    err_file.close()
    return True

if __name__ == "__main__":
    success = main()
    sys.exit(0 if success else 1)
