"""Browser smoke tests against Vite preview, with contract-shaped same-origin APIs.

Run with Python Playwright installed and a preview server on 127.0.0.1:4178.
The test never talks to a deployed upfile service.
"""
import json
import re
import time
from pathlib import Path

from playwright.sync_api import sync_playwright, expect

BASE = "http://127.0.0.1:4178"
NOW = int(time.time())
Path("test-results").mkdir(exist_ok=True)


def json_response(route, value, status=200):
    route.fulfill(status=status, content_type="application/json", body=json.dumps(value))


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True, channel="chrome")
    context = browser.new_context(viewport={"width": 1280, "height": 900})
    page = context.new_page()
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    settings = {
        "configured": False, "max_file_bytes": 1024, "storage_budget_bytes": 10240,
        "default_link_hours": 168, "stored_bytes": 5, "reserved_bytes": 0,
        "chunk_bytes": 8, "lease_seconds": 120, "max_records": 10000,
        "record_count": 3, "cleanup_errors": 0,
    }
    containers = []
    links = []
    files = [{
        "id": "f1", "container_id": "c1", "link_id": "a1", "name": "<img onerror=alert(1)>.txt",
        "original_name": "<img onerror=alert(1)>.txt", "sender_label": "Alex", "comment": "<script>not executable</script>",
        "size": 5, "created_at": NOW, "status": "ready",
    }]
    observed = []

    def admin_api(route):
        request = route.request
        path = request.url.split(BASE)[1].split("?")[0]
        method = request.method
        observed.append((method, path))
        if method not in ("GET", "HEAD"):
            assert request.headers["x-upfile-request"] == "1"
        payload = request.post_data_json if request.post_data else {}
        if path == "/api/surface":
            return json_response(route, {"surface": "admin"})
        if path == "/api/settings":
            if method == "PUT":
                settings.update(payload)
                settings["configured"] = True
            return json_response(route, settings)
        if path == "/api/containers":
            if method == "POST":
                item = dict(payload, id="c1", status="active", created_at=NOW, file_count=len(files),
                            stored_bytes=5, active_uploads=2, active_downloads=3, link_count=1,
                            effective_max_bytes=1024)
                containers.append(item)
                initial_link = dict(id="a1", container_id="c1", sender_label="Default link",
                                    max_file_bytes=None, effective_max_bytes=1024,
                                    expires_at=NOW + settings["default_link_hours"] * 3600,
                                    status="active", created_at=NOW, file_count=0, active_uploads=0)
                links.append(initial_link)
                return json_response(route, dict(item, initial_link=dict(
                    initial_link, url=f"{BASE}/u/a1#private-secret")))
            return json_response(route, {"items": containers, "total": len(containers)})
        if path == "/api/containers/c1":
            if method == "PUT":
                containers[0].update(payload)
            if method == "DELETE":
                containers.clear()
                return json_response(route, {"status": "deleted"})
            return json_response(route, containers[0])
        if path == "/api/containers/c1/links":
            if method == "POST":
                item = dict(payload, id="a1", container_id="c1", status="active", created_at=NOW,
                            file_count=0, active_uploads=1, effective_max_bytes=1024)
                links.append(item)
                containers[0]["link_count"] += 1
                return json_response(route, dict(item, url=f"{BASE}/u/a1#private-secret"))
            return json_response(route, {"items": links, "total": len(links)})
        if path.startswith("/api/links/a1"):
            if path.endswith("/revoke"):
                links[0]["status"] = "revoked"
                return json_response(route, {"status": "revoked"})
            if path.endswith("/rotate"):
                return json_response(route, dict(links[0], url=f"{BASE}/u/a1#replacement-secret"))
            links[0].update(payload)
            return json_response(route, links[0])
        if path == "/api/containers/c1/files":
            return json_response(route, {"items": files, "total": len(files)})
        if path == "/api/files/f1":
            if method == "PUT":
                files[0].update(payload)
                return json_response(route, files[0])
            files.clear()
            containers[0]["file_count"] = 0
            return json_response(route, {"status": "deleted"})
        if path == "/api/files/f1/download":
            return route.fulfill(status=200, content_type="application/octet-stream",
                                 headers={"Content-Disposition": 'attachment; filename="delivery.txt"'}, body="hello")
        if path == "/api/audit":
            return json_response(route, {"items": [{"id": "d1", "actor": "admin", "action": "created", "target": "c1", "created_at": NOW}], "total": 1})
        raise AssertionError(f"Unexpected admin API: {method} {path}")

    context.route("**/api/**", admin_api)
    page.goto(BASE)
    page.wait_for_load_state("networkidle")
    expect(page.get_by_role("heading", name="Welcome to upfile")).to_be_visible()
    expect(page.get_by_label("Global maximum file size")).to_have_value("")
    expect(page.get_by_label("Total storage budget")).to_have_value("")
    page.get_by_label("Global maximum file size").fill("0.001024")
    page.get_by_label("Total storage budget").fill("0.01024")
    page.get_by_role("button", name="Save and get started").click()
    expect(page.get_by_role("heading", name="File requests", exact=True)).to_be_visible()
    assert settings["max_file_bytes"] == 1024
    assert settings["storage_budget_bytes"] == 10240
    page.get_by_role("link", name="Requests", exact=True).click()
    page.get_by_role("button", name="New request").click()
    page.get_by_label("Request name").fill("Spring handoff")
    page.get_by_label("Public instructions").fill("Send the originals. <script>plain text</script>")
    page.get_by_role("button", name="Create request", exact=True).click()
    expect(page.get_by_label("Private upload link")).to_have_value(BASE + "/u/a1#private-secret")
    page.get_by_role("button", name="Copy link", exact=True).click()
    expect(page.get_by_text("Link copied.", exact=True)).to_be_visible()
    page.get_by_role("button", name="Done", exact=True).click()
    page.get_by_role("link", name="Spring handoff", exact=True).click()
    expect(page.get_by_role("heading", name="Spring handoff", exact=True)).to_be_visible()
    page.get_by_role("tab", name="Upload links").click()
    page.get_by_role("button", name="Replace", exact=True).click()
    page.get_by_role("button", name="Replace link", exact=True).click()
    expect(page.get_by_label("Private upload link")).to_have_value(BASE + "/u/a1#replacement-secret")
    page.get_by_role("button", name="Done", exact=True).click()
    page.get_by_role("button", name="Revoke", exact=True).click()
    page.get_by_role("button", name="Revoke link", exact=True).click()
    expect(page.get_by_text("Link revoked. Received files were kept.", exact=True)).to_be_visible()
    page.get_by_role("tab", name="Received files").click()
    page.get_by_text("Comment", exact=True).click()
    expect(page.get_by_text("<script>not executable</script>", exact=True)).to_be_visible()
    assert page.locator("img").count() == 0
    page.get_by_role("button", name="Rename", exact=True).click()
    page.get_by_label("Display filename").fill("delivery.txt")
    page.get_by_role("button", name="Save changes", exact=True).click()
    expect(page.get_by_text("delivery.txt", exact=True)).to_be_visible()
    page.get_by_role("button", name="Download", exact=True).click()
    with page.expect_download() as downloaded:
        page.get_by_role("link", name="Download file", exact=True).click()
    # Chromium native downloads bypass Playwright routing; the backend owns attachment headers.
    assert downloaded.value.url == BASE + "/api/files/f1/download"
    page.screenshot(path="test-results/admin.png", full_page=True)
    page.get_by_role("button", name="Delete", exact=True).click()
    expect(page.get_by_text("active downloads and", exact=False)).to_be_visible()
    page.get_by_role("button", name="Delete file", exact=True).click()
    expect(page.get_by_text("File deleted. Its upload link is unchanged.", exact=True)).to_be_visible()
    page.get_by_role("button", name="Delete request", exact=True).click()
    page.get_by_label("Type Spring handoff to confirm").fill("Spring handoff")
    page.get_by_role("button", name="Delete request and contents", exact=True).click()
    expect(page.get_by_role("heading", name="Request deleted", exact=True)).to_be_visible()
    assert not errors, errors
    context.close()

    context = browser.new_context(viewport={"width": 390, "height": 844})
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    events = []
    attempts = {}
    exchange_count = 0
    lost_admission = True
    hold_upload = False
    missing_upload = False
    expired_session = False
    held_routes = []
    failed_requests = []
    page.on("requestfailed", lambda request: failed_requests.append(request.url))
    public_info = {
        "id": "a1", "title": "Spring handoff", "instructions": "Send the originals. <script>plain text</script>",
        "max_file_bytes": 1024, "expires_at": NOW + 3600, "session_expires_at": NOW + 1800,
        "chunk_bytes": 8, "busy": False, "reset_available": False,
    }

    def public_api(route):
        global exchange_count, lost_admission
        request = route.request
        path = request.url.split(BASE)[1].split("?")[0]
        method = request.method
        events.append((method, path))
        if method not in ("GET", "HEAD"):
            assert request.headers["x-upfile-request"] == "1"
        if path == "/api/surface":
            return json_response(route, {"surface": "public"})
        if path.endswith("/exchange"):
            exchange_count += 1
            assert request.post_data_json == {"secret": "secret-fragment"}
            assert page.url == BASE + "/u/a1"
            return json_response(route, public_info)
        if path == "/api/links/a1":
            if expired_session:
                return json_response(route, {"code": "session_required", "error": "Reopen the original link."}, 401)
            return json_response(route, public_info)
        if path.endswith("/attempts"):
            payload = request.post_data_json
            key = payload["key"]
            if key not in attempts:
                ident = f"b{len(attempts) + 1}"
                attempts[key] = dict(id=ident, status="uploading", size=payload["size"], offset=0,
                                     upload_url=f"{BASE}/api/links/a1/uploads/{ident}", created_at=NOW,
                                     comment=payload["comment"])
            if lost_admission:
                lost_admission = False
                return route.abort("failed")
            return json_response(route, attempts[key])
        if path.endswith("/cancel"):
            ident = path.split("/")[-2]
            attempt = next(value for value in attempts.values() if value["id"] == ident)
            attempt["status"] = "canceled"
            return json_response(route, {"status": "canceled"})
        ident = path.split("/")[-1]
        attempt = next((value for value in attempts.values() if value["id"] == ident), None)
        assert attempt is not None, f"Unexpected public API: {method} {path}"
        if "/uploads/" in path:
            assert method in ("HEAD", "PATCH"), "No tus creation or DELETE allowed"
            assert "upload-metadata" not in request.headers
            if missing_upload:
                return route.fulfill(status=404)
            if hold_upload and method == "PATCH":
                held_routes.append(route)
                return
            headers = {"Tus-Resumable": "1.0.0", "Upload-Offset": str(attempt["offset"]), "Upload-Length": str(attempt["size"])}
            if method == "PATCH":
                assert int(request.headers["upload-offset"]) == attempt["offset"]
                attempt["offset"] += len(request.post_data_buffer or b"")
                if attempt["offset"] == attempt["size"]:
                    attempt["status"] = "completed"
                headers["Upload-Offset"] = str(attempt["offset"])
            return route.fulfill(status=200 if method == "HEAD" else 204, headers=headers)
        return json_response(route, attempt)

    page.route("**/api/**", public_api)
    page.goto(BASE + "/u/a1#secret-fragment")
    page.wait_for_load_state("networkidle")
    expect(page.get_by_role("heading", name="Spring handoff", exact=True)).to_be_visible()
    assert exchange_count == 1
    assert page.evaluate("location.hash") == ""
    assert page.evaluate("Object.keys(localStorage).length") == 0
    assert page.locator("script:not([src])").count() == 0
    page.get_by_label("Choose files").set_input_files([
        {"name": "<img onerror=alert(1)>.txt", "mimeType": "text/plain", "buffer": b"first file"},
        {"name": "second.txt", "mimeType": "text/plain", "buffer": b"second file"},
    ])
    page.locator("textarea").first.fill("A plain-text note.")
    page.get_by_role("button", name=re.compile(r"Send 2 files")).click()
    expect(page.get_by_text("2 received · 0 failed · 0 canceled · 0 waiting", exact=True)).to_be_visible(timeout=15000)
    assert len(attempts) == 2, "Lost admission response must not allocate a second attempt"
    assert len([event for event in events if event == ("POST", "/api/links/a1/attempts")]) == 3
    first_receipt = events.index(("GET", "/api/links/a1/attempts/b1"))
    second_head = events.index(("HEAD", "/api/links/a1/uploads/b2"))
    assert first_receipt < second_head
    assert page.locator("img").count() == 0
    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth")
    page.screenshot(path="test-results/public-mobile.png", full_page=True)
    page.get_by_role("button", name="Clear finished").click()
    page.get_by_label("Choose files").set_input_files({"name": "again.txt", "mimeType": "text/plain", "buffer": b"again"})
    page.get_by_role("button", name=re.compile(r"Send 1 file")).click()
    expect(page.get_by_text("1 received · 0 failed · 0 canceled · 0 waiting", exact=True)).to_be_visible()
    page.reload()
    expect(page.get_by_text("No files selected yet.", exact=True)).to_be_visible()
    assert exchange_count == 1
    hold_upload = True
    page.get_by_label("Choose files").set_input_files({"name": "cancel.txt", "mimeType": "text/plain", "buffer": b"cancel me"})
    page.get_by_role("button", name=re.compile(r"Send 1 file")).click()
    expect(page.get_by_role("button", name="Cancel", exact=True)).to_be_visible()
    page.wait_for_function("document.querySelector('progress') !== null")
    page.get_by_role("button", name="Cancel", exact=True).click()
    expect(page.get_by_text("0 received · 0 failed · 1 canceled · 0 waiting", exact=True)).to_be_visible()
    for held_route in held_routes:
        held_route.abort("aborted")
    assert ("POST", "/api/links/a1/attempts/b4/cancel") in events
    assert all(method != "DELETE" for method, _ in events)
    hold_upload = False
    page.get_by_role("button", name="Clear finished").click()
    missing_upload = True
    page.get_by_label("Choose files").set_input_files({"name": "missing.txt", "mimeType": "text/plain", "buffer": b"missing"})
    page.get_by_role("button", name=re.compile(r"Send 1 file")).click()
    expect(page.get_by_text("0 received · 1 failed · 0 canceled · 0 waiting", exact=True)).to_be_visible()
    assert all(not (method == "POST" and "/uploads/" in path) for method, path in events)
    assert len(attempts) == 5
    expired_session = True
    page.reload()
    expect(page.get_by_text("Your upload session is missing or expired.", exact=False)).to_be_visible()
    assert exchange_count == 1
    assert not errors, errors
    context.close()
    browser.close()
    print("Admin CRUD/downloads, private links, safe text, serial tus, retries, cancellation, missing resources, expiry, reuse, and mobile checks passed.")
