"""验证社交分发消息的格式和签名。"""
import json
import unittest

import social


class SocialPayloadTests(unittest.TestCase):
    def setUp(self):
        self.release = {
            "tagName": "backend-v1.2.3",
            "name": "水杉输入法后端 v1.2.3",
            "body": "### 修复\n- 合成修复",
            "url": "https://github.com/metasequoiaime/msime-cloud/releases/tag/backend-v1.2.3",
            "publishedAt": "2026-10-28T00:00:00Z",
            "targetCommitish": "abc123",
        }

    def test_payload_has_stable_idempotency_key_and_release_fields(self):
        payload = social.build_payload("metasequoiaime/msime-cloud", "backend-v1.2.3", self.release)
        self.assertEqual(payload["schema_version"], 1)
        self.assertEqual(payload["event"], "release.published")
        self.assertEqual(payload["idempotency_key"], "metasequoiaime/msime-cloud:backend-v1.2.3")
        self.assertEqual(payload["release"]["tag"], "backend-v1.2.3")
        self.assertEqual(payload["release"]["body"], self.release["body"])

    def test_tag_mismatch_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "标签"):
            social.build_payload("repo", "backend-v9.9.9", self.release)

    def test_signature_is_hmac_of_exact_body(self):
        payload = social.build_payload("repo", "backend-v1.2.3", self.release)
        body = social.encode_payload(payload)
        self.assertEqual(
            social.signature(body, "secret"),
            "sha256=f4699336bac42e6bf8709aa3dec4340292d7b6e1162a6f4c31de687a76378acc",
        )
        self.assertEqual(json.loads(body), payload)


if __name__ == "__main__":
    unittest.main()
