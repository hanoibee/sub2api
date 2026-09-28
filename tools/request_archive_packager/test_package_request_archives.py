import argparse
import datetime as dt
import json
import os
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import package_request_archives as packager


class PackageRequestArchivesTest(unittest.TestCase):
    def make_request(self, root, date, sequence, request_time, payload=b"payload") -> str:
        name = f"req_{date.replace('-', '')}_{sequence:06d}"
        request = root / date / name
        request.mkdir(parents=True)
        (request / "metadata.json").write_text(
            json.dumps({"requestTime": request_time}), encoding="utf-8"
        )
        (request / "biz_request.json").write_bytes(payload)
        (request / "biz_response.json").write_bytes(payload)
        parsed_time = dt.datetime.strptime(request_time, "%Y-%m-%d %H:%M:%S").replace(
            tzinfo=packager.UTC_PLUS_8
        )
        os.utime(request, (parsed_time.timestamp(), parsed_time.timestamp()))
        return name

    def run_ready(self, archive_root, maas_root, cutoff, max_size=1024 * 1024):
        with mock.patch.object(packager, "MAAS_DATA_ROOT", maas_root):
            return packager.package_ready_requests(archive_root, "provider-a", cutoff, max_size)

    def test_incrementally_packages_only_requests_before_cutoff(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            first = self.make_request(root, "2026-09-23", 1, "2026-09-23 10:00:00")
            second = self.make_request(root, "2026-09-23", 2, "2026-09-23 10:40:00")
            # 即使后续序号的目录时间更早，也不能越过序号 2 先行打包。
            third = self.make_request(root, "2026-09-23", 3, "2026-09-23 09:00:00")
            cutoff = dt.datetime(2026, 9, 23, 10, 30, tzinfo=packager.UTC_PLUS_8)

            self.assertEqual(self.run_ready(root, maas, cutoff), (1, 1))
            output = maas / "archive" / "provider-a" / "2026-09-23"
            first_record = packager.load_manifest(output)["batches"][0]
            self.assertEqual(first_record["first_request"], first)
            self.assertNotIn("requests", first_record)
            self.assertFalse((root / "2026-09-23" / first).exists())
            self.assertTrue((root / "2026-09-23" / second).exists())
            self.assertTrue((root / "2026-09-23" / third).exists())

            later = dt.datetime(2026, 9, 23, 11, 30, tzinfo=packager.UTC_PLUS_8)
            self.assertEqual(self.run_ready(root, maas, later), (2, 1))
            manifest = packager.load_manifest(output)
            self.assertEqual(
                [item["file"] for item in manifest["batches"]],
                ["batch_000001.tar.gz", "batch_000002.tar.gz"],
            )
            self.assertEqual(manifest["batches"][1]["first_request"], second)
            self.assertEqual(manifest["batches"][1]["last_request"], third)
            self.assertFalse((root / "2026-09-23" / second).exists())
            self.assertFalse((root / "2026-09-23" / third).exists())
            self.assertEqual(self.run_ready(root, maas, later), (0, 0))

    def test_never_mixes_requests_from_different_dates(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            self.make_request(root, "2026-09-23", 1, "2026-09-23 23:59:59")
            self.make_request(root, "2026-09-24", 1, "2026-09-24 00:00:00")
            cutoff = dt.datetime(2026, 9, 24, 1, 0, tzinfo=packager.UTC_PLUS_8)

            self.assertEqual(self.run_ready(root, maas, cutoff), (2, 2))
            for date in ("2026-09-23", "2026-09-24"):
                batch = maas / "archive" / "provider-a" / date / "batch_000001.tar.gz"
                with tarfile.open(batch, "r:gz") as archive:
                    top = [m.name for m in archive.getmembers() if m.isdir() and "/" not in m.name]
                self.assertEqual(len(top), 1)
                self.assertTrue(top[0].startswith(f"req_{date.replace('-', '')}_"))
                self.assertEqual(list((root / date).glob("req_*")), [])

    def test_numeric_sequence_order_is_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            expected = [
                self.make_request(root, "2026-09-23", seq, "2026-09-23 10:00:00")
                for seq in (999998, 999999, 1000000, 1000001)
            ]
            cutoff = dt.datetime(2026, 9, 23, 11, 0, tzinfo=packager.UTC_PLUS_8)
            self.run_ready(root, maas, cutoff)
            batch = maas / "archive" / "provider-a" / "2026-09-23" / "batch_000001.tar.gz"
            with tarfile.open(batch, "r:gz") as archive:
                actual = [m.name for m in archive.getmembers() if m.isdir() and "/" not in m.name]
            self.assertEqual(actual, expected)

    def test_single_oversize_request_is_published_separately(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            self.make_request(root, "2026-09-23", 1, "2026-09-23 10:00:00", b"small")
            self.make_request(root, "2026-09-23", 2, "2026-09-23 10:01:00", os.urandom(4096))
            cutoff = dt.datetime(2026, 9, 23, 11, 0, tzinfo=packager.UTC_PLUS_8)

            self.assertEqual(self.run_ready(root, maas, cutoff, 2000), (2, 2))
            output = maas / "archive" / "provider-a" / "2026-09-23"
            batches = sorted(output.glob("batch_*.tar.gz"))
            self.assertLess(batches[0].stat().st_size, 2000)
            self.assertGreaterEqual(batches[1].stat().st_size, 2000)

    def test_recovers_batch_published_before_manifest_update(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            self.make_request(root, "2026-09-23", 1, "2026-09-23 10:00:00")
            output = maas / "archive" / "provider-a" / "2026-09-23"
            staging = Path(temp) / "staging"
            output.mkdir(parents=True)
            staging.mkdir()
            requests = packager.discover_requests(root / "2026-09-23", "2026-09-23")
            records = packager.build_batches(requests, staging, 1024 * 1024)
            (staging / records[0]["file"]).replace(output / records[0]["file"])

            cutoff = dt.datetime(2026, 9, 23, 11, 0, tzinfo=packager.UTC_PLUS_8)
            self.assertEqual(self.run_ready(root, maas, cutoff), (0, 0))
            manifest = packager.load_manifest(output)
            self.assertEqual(manifest["request_count"], 1)
            self.assertEqual(manifest["version"], 3)
            self.assertNotIn("requests", manifest["batches"][0])
            self.assertFalse((root / "2026-09-23" / "req_20260923_000001").exists())

    def test_corrupt_orphan_batch_never_deletes_source(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            name = self.make_request(root, "2026-09-23", 1, "2026-09-23 10:00:00")
            output = maas / "archive" / "provider-a" / "2026-09-23"
            output.mkdir(parents=True)
            (output / "batch_000001.tar.gz").write_bytes(b"not a valid archive")
            cutoff = dt.datetime(2026, 9, 23, 11, 0, tzinfo=packager.UTC_PLUS_8)

            with self.assertRaises(packager.PackagingError):
                self.run_ready(root, maas, cutoff)
            self.assertTrue((root / "2026-09-23" / name).is_dir())

    def test_migrates_v2_manifest_and_deletes_original_source(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root, maas = Path(temp) / "requests", Path(temp) / "maasData"
            name = self.make_request(root, "2026-09-23", 1, "2026-09-23 10:00:00")
            output = maas / "archive" / "provider-a" / "2026-09-23"
            staging = Path(temp) / "staging"
            output.mkdir(parents=True)
            staging.mkdir()
            requests = packager.discover_requests(root / "2026-09-23", "2026-09-23")
            record = packager.build_batches(requests, staging, 1024 * 1024)[0]
            (staging / record["file"]).replace(output / record["file"])
            manifest = {
                "version": 2,
                "date": "2026-09-23",
                "provider_code": "provider-a",
                "max_size_bytes": 1024 * 1024,
                "batches": [record],
            }
            (output / packager.BATCH_MANIFEST).write_text(json.dumps(manifest), encoding="utf-8")
            cutoff = dt.datetime(2026, 9, 23, 11, 0, tzinfo=packager.UTC_PLUS_8)

            self.assertEqual(self.run_ready(root, maas, cutoff), (0, 0))
            migrated = packager.load_manifest(output)
            self.assertEqual(migrated["version"], 3)
            self.assertNotIn("requests", migrated["batches"][0])
            self.assertFalse((root / "2026-09-23" / name).exists())

    def test_parse_configuration_values(self) -> None:
        self.assertEqual(packager.parse_size("100M"), 100 * 1024 * 1024)
        self.assertEqual(packager.parse_duration("30m"), 1800)
        with self.assertRaises(argparse.ArgumentTypeError):
            packager.validate_provider_code("../provider")


if __name__ == "__main__":
    unittest.main()
