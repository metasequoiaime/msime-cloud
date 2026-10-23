import io
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
import unittest
from unittest.mock import patch
import urllib.error

import qq_command_panel as panel


class PanelRequestTests(unittest.TestCase):
    def test_redirect_does_not_forward_bot_token(self):
        forwarded = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", f"http://localhost:{self.server.server_port}/target")
                    self.end_headers()
                else:
                    forwarded.append(self.headers.get("Authorization"))
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(b"{}")

            def log_message(self, *args):
                pass

        with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                with patch.dict(os.environ, {"no_proxy": "*", "NO_PROXY": "*"}):
                    try:
                        panel.call("GET", f"http://127.0.0.1:{server.server_port}/redirect", token="synthetic-bot-token")
                    except SystemExit as error:
                        diagnostic = str(error)
                    else:
                        diagnostic = ""
                self.assertEqual(forwarded, [])
                self.assertIn("HTTP 302", diagnostic)
            finally:
                server.shutdown()
                worker.join()

    def test_http_error_does_not_echo_response_body(self):
        url = "https://example.test/panels"
        for response in (b'{"message":"synthetic-secret-marker"}', b'\xffsynthetic-secret-marker'):
            with self.subTest(response=response):
                body = io.BytesIO(response)
                error = urllib.error.HTTPError(url, 401, "unauthorized", {}, body)
                with patch.object(panel.urllib.request.OpenerDirector, "open", side_effect=error):
                    with self.assertRaises(SystemExit) as raised:
                        panel.call("POST", url, {"clientSecret": "synthetic-secret-marker"})
                self.assertEqual(str(raised.exception), f"POST {url}: HTTP 401")
                self.assertTrue(body.closed)


if __name__ == "__main__":
    unittest.main()
