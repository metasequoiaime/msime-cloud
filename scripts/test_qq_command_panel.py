import io
import unittest
from unittest.mock import patch
import urllib.error

import qq_command_panel as panel


class PanelRequestTests(unittest.TestCase):
    def test_http_error_does_not_echo_response_body(self):
        url = "https://example.test/panels"
        for response in (b'{"message":"synthetic-secret-marker"}', b'\xffsynthetic-secret-marker'):
            with self.subTest(response=response):
                body = io.BytesIO(response)
                error = urllib.error.HTTPError(url, 401, "unauthorized", {}, body)
                with patch.object(panel.urllib.request, "urlopen", side_effect=error):
                    with self.assertRaises(SystemExit) as raised:
                        panel.call("POST", url, {"clientSecret": "synthetic-secret-marker"})
                self.assertEqual(str(raised.exception), f"POST {url}: HTTP 401")
                self.assertTrue(body.closed)


if __name__ == "__main__":
    unittest.main()
