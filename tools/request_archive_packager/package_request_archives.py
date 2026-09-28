#!/usr/bin/env python3
"""将一天的请求归档打包为有大小上限的多个 tar.gz 文件。"""

from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import gzip
import hashlib
import json
import os
import re
import shutil
import sys
import tarfile
import tempfile
import time
from contextlib import contextmanager
from pathlib import Path, PurePosixPath
from typing import Sequence


# 请求归档固定使用 UTC+08:00 分日，不能直接跟随服务器本地时区。
UTC_PLUS_8 = dt.timezone(dt.timedelta(hours=8), name="UTC+08:00")
# 压缩归档固定发布在 /maasData/archive/{providerCode}/{yyyy-MM-dd}/。
MAAS_DATA_ROOT = Path("/maasData")
# 常规多请求批次必须严格小于此上限；不可拆分的单个大请求允许例外。
DEFAULT_MAX_SIZE = 100 * 1024 * 1024
# 默认在 96 MiB 左右停止追加新请求，为最后一个请求及 tar/gzip 收尾预留空间。
DEFAULT_HEADROOM = 4 * 1024 * 1024
REQUEST_DIRECTORY_RE = re.compile(r"^req_(\d{8})_(\d{6,})$")
BATCH_FILE_RE = re.compile(r"^batch_(\d{6,})\.tar\.gz$")
BATCH_MANIFEST = "batch_manifest.json"
SIZE_RE = re.compile(r"^(\d+)([KMG]i?B?|B)?$", re.IGNORECASE)
DATE_DIRECTORY_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")
DURATION_RE = re.compile(r"^(\d+)([smh])$", re.IGNORECASE)


class PackagingError(RuntimeError):
    """可预期的归档打包错误，入口函数会将其转换为非零退出码。"""

    pass


def parse_size(value: str) -> int:
    """将 100M、100MiB 等参数转换为字节数，所有单位均按 1024 进制。"""

    match = SIZE_RE.fullmatch(value.strip())
    if not match:
        raise argparse.ArgumentTypeError(f"invalid size: {value!r}")
    amount = int(match.group(1))
    unit = (match.group(2) or "B").upper()
    multipliers = {
        "B": 1,
        "K": 1024,
        "KB": 1024,
        "KIB": 1024,
        "M": 1024**2,
        "MB": 1024**2,
        "MIB": 1024**2,
        "G": 1024**3,
        "GB": 1024**3,
        "GIB": 1024**3,
    }
    if amount <= 0:
        raise argparse.ArgumentTypeError("size must be positive")
    return amount * multipliers[unit]


def validate_provider_code(value: str) -> str:
    """校验 providerCode 可安全用作单层目录名。"""

    if not value or value != value.strip():
        raise argparse.ArgumentTypeError("provider code must not be empty or contain outer spaces")
    if value in {".", ".."} or "/" in value or "\\" in value or "\0" in value:
        raise argparse.ArgumentTypeError("provider code must be a single safe path segment")
    return value


def parse_duration(value: str) -> int:
    """将 30s、30m、2h 等周期参数转换为秒数。"""

    match = DURATION_RE.fullmatch(value.strip())
    if not match:
        raise argparse.ArgumentTypeError("duration must look like 30s, 30m, or 2h")
    amount = int(match.group(1))
    if amount <= 0:
        raise argparse.ArgumentTypeError("duration must be positive")
    return amount * {"s": 1, "m": 60, "h": 3600}[match.group(2).lower()]


def discover_requests(source_dir: Path, date: str) -> list[Path]:
    """发现指定日期的正式请求目录，并排除脚本自身生成的文件。"""

    if not source_dir.is_dir():
        raise PackagingError(f"source date directory does not exist: {source_dir}")
    date_id = date.replace("-", "")
    requests: list[Path] = []
    unexpected: list[str] = []
    for entry in source_dir.iterdir():
        # 隐藏文件可能是其他程序的临时文件；本脚本的批次和清单也不是输入。
        if entry.name.startswith("."):
            continue
        if entry.name == BATCH_MANIFEST or BATCH_FILE_RE.fullmatch(entry.name):
            continue
        match = REQUEST_DIRECTORY_RE.fullmatch(entry.name)
        if not match or match.group(1) != date_id:
            unexpected.append(entry.name)
            continue
        if entry.is_symlink() or not entry.is_dir():
            raise PackagingError(f"request archive is not a real directory: {entry}")
        requests.append(entry)
    if unexpected:
        # 对未知文件采取失败策略，避免它们被悄悄漏掉或误打包。
        names = ", ".join(sorted(unexpected)[:5])
        raise PackagingError(f"unexpected entries in source date directory: {names}")
    # 目录末尾序号代表请求发生的先后顺序，必须按数值排序。不能直接按文件名
    # 排序，否则序号扩展到七位后，1000000 会错误地排在 999999 前面。
    requests.sort(key=lambda path: int(REQUEST_DIRECTORY_RE.fullmatch(path.name).group(2)))
    return requests


def file_sha256(path: Path) -> str:
    """流式计算文件摘要，避免一次把接近 100 MiB 的包读入内存。"""

    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def add_request(tar: tarfile.TarFile, request: Path) -> None:
    """把一个完整请求目录加入 tar，并拒绝符号链接。"""

    def reject_links(info: tarfile.TarInfo) -> tarfile.TarInfo:
        if info.issym() or info.islnk():
            raise PackagingError(f"links are not allowed: {info.name}")
        return info

    tar.add(request, arcname=request.name, recursive=True, filter=reject_links)


def write_archive(path: Path, requests: Sequence[Path], soft_limit: int | None = None) -> int:
    """将给定请求写入一个 gzip 压缩的 tar 文件，并返回最终字节数。"""

    with path.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(mode="w|", fileobj=compressed, dereference=False) as tar:
                for index, request in enumerate(requests):
                    add_request(tar, request)
                    if soft_limit is not None:
                        compressed.flush()
                        raw.flush()
                        if raw.tell() >= soft_limit and index + 1 < len(requests):
                            break
    return path.stat().st_size


def build_batches(
    requests: Sequence[Path], staging: Path, max_size: int, start_number: int = 1
) -> list[dict[str, object]]:
    """按顺序分包；多请求包小于上限，单个不可拆分的大请求允许超限。"""

    batches: list[dict[str, object]] = []
    remaining = list(requests)
    headroom = min(DEFAULT_HEADROOM, max(1, max_size // 20))
    soft_limit = max(1, max_size - headroom)

    while remaining:
        batch_number = start_number + len(batches)
        final_name = f"batch_{batch_number:06d}.tar.gz"
        candidate = staging / f".{final_name}.candidate"

        # 第一遍持续追加请求并观察压缩后的实际大小。接近上限时停止，避免为每加入
        # 一个请求都从头压缩一次；JSON/SSE 的压缩率无法仅凭源文件大小准确估算。
        with candidate.open("wb") as raw:
            consumed = 0
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(mode="w|", fileobj=compressed, dereference=False) as tar:
                    for request in remaining:
                        add_request(tar, request)
                        consumed += 1
                        compressed.flush()
                        raw.flush()
                        if raw.tell() >= soft_limit:
                            break

        chosen = remaining[:consumed]
        size = candidate.stat().st_size
        # gzip/tar 关闭时还会写入尾部，因此第一遍的候选包仍可能越界。
        # 若越界，则逐个退回末尾请求并重建，直到严格小于上限；仅剩一个
        # 不可拆分的请求时允许超限发布。
        while size >= max_size:
            candidate.unlink(missing_ok=True)
            if len(chosen) == 1:
                # 请求目录是不可拆分的最小单位。若单个请求本身压缩后仍然超限，
                # 允许它独占一个批次发布，避免整天归档因一个大请求无法产出。
                size = write_archive(candidate, chosen)
                break
            chosen = chosen[:-1]
            size = write_archive(candidate, chosen)

        final_path = staging / final_name
        candidate.replace(final_path)
        batches.append(
            {
                "file": final_name,
                "size": size,
                "sha256": file_sha256(final_path),
                "request_count": len(chosen),
                "first_request": chosen[0].name,
                "last_request": chosen[-1].name,
                # requests 只在发布和删除原目录期间使用，不会写入正式清单。
                "requests": [request.name for request in chosen],
            }
        )
        remaining = remaining[len(chosen) :]
    return batches


def load_manifest(output_dir: Path) -> dict[str, object] | None:
    """读取增量批次清单；不存在时表示该日期尚无已登记批次。"""

    manifest_path = output_dir / BATCH_MANIFEST
    if not manifest_path.is_file():
        return None
    try:
        return json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise PackagingError(f"cannot read existing manifest: {manifest_path}: {exc}") from exc


def batch_number(name: str) -> int:
    """从 batch_000001.tar.gz 中解析可扩展位数的批次号。"""

    match = BATCH_FILE_RE.fullmatch(name)
    if not match:
        raise PackagingError(f"invalid batch file name: {name}")
    return int(match.group(1))


def verify_batch(path: Path, date: str, expected_names: Sequence[str] | None = None) -> list[str]:
    """完整读取并校验 tar.gz，返回其中准确的请求目录名。"""

    date_id = date.replace("-", "")
    seen: set[str] = set()
    try:
        with tarfile.open(path, "r:gz") as archive:
            for member in archive:
                member_path = PurePosixPath(member.name)
                if member_path.is_absolute() or ".." in member_path.parts or not member_path.parts:
                    raise PackagingError(f"unsafe member path in batch: {path}: {member.name}")
                top = member_path.parts[0]
                match = REQUEST_DIRECTORY_RE.fullmatch(top)
                if not match or match.group(1) != date_id:
                    raise PackagingError(
                        f"batch contains request from another date: {path}: {member.name}"
                    )
                if member.issym() or member.islnk():
                    raise PackagingError(f"links are not allowed in batch: {path}: {member.name}")
                seen.add(top)
                # 读取每个普通文件到 EOF，触发 gzip CRC 和截断校验，而不落盘解压。
                if member.isfile():
                    source = archive.extractfile(member)
                    if source is None:
                        raise PackagingError(f"cannot read batch member: {path}: {member.name}")
                    while source.read(1024 * 1024):
                        pass
    except (OSError, EOFError, tarfile.TarError) as exc:
        raise PackagingError(f"invalid or truncated batch: {path}: {exc}") from exc
    if not seen:
        raise PackagingError(f"batch contains no request directories: {path}")
    names = sorted(seen, key=lambda name: int(REQUEST_DIRECTORY_RE.fullmatch(name).group(2)))
    if expected_names is not None and names != list(expected_names):
        raise PackagingError(f"batch request list does not match source selection: {path}")
    return names


def batch_record(path: Path, date: str) -> dict[str, object]:
    """完整校验一个未登记批次并重建临时记录。"""

    names = verify_batch(path, date)
    return {
        "file": path.name,
        "size": path.stat().st_size,
        "sha256": file_sha256(path),
        "request_count": len(names),
        "first_request": names[0],
        "last_request": names[-1],
        "requests": names,
    }


def public_record(record: dict[str, object]) -> dict[str, object]:
    """移除仅供删除阶段使用的请求名，生成精简的 v3 清单记录。"""

    return {
        key: record[key]
        for key in ("file", "size", "sha256", "request_count", "first_request", "last_request")
    }


def fsync_file(path: Path) -> None:
    """确保已经关闭的压缩包内容持久化到存储设备。"""

    with path.open("rb") as file:
        os.fsync(file.fileno())


def fsync_directory(path: Path) -> None:
    """确保目录项变更持久化；不支持目录 fsync 的平台返回错误。"""

    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def delete_source_requests(source_dir: Path, names: Sequence[str], date: str) -> None:
    """删除已被可靠发布的请求目录；所有目标都必须通过严格路径校验。"""

    date_id = date.replace("-", "")
    source_dir = source_dir.resolve()
    for name in names:
        match = REQUEST_DIRECTORY_RE.fullmatch(name)
        if not match or match.group(1) != date_id:
            raise PackagingError(f"refusing to delete invalid request directory name: {name}")
        target = source_dir / name
        if target.is_symlink():
            raise PackagingError(f"refusing to delete symbolic link: {target}")
        if not target.exists():
            continue
        if not target.is_dir() or target.parent != source_dir:
            raise PackagingError(f"refusing to delete unsafe request directory: {target}")
        shutil.rmtree(target)
    fsync_directory(source_dir)


def load_batch_state(
    output_dir: Path,
    source_dir: Path,
    date: str,
    provider_code: str,
    max_size: int,
) -> tuple[list[dict[str, object]], int]:
    """读取 v3 状态，迁移 v2，并完成未登记批次的删除与登记。"""

    manifest = load_manifest(output_dir)
    records: list[dict[str, object]] = []
    manifest_version = 3
    if manifest is not None:
        manifest_version = manifest.get("version")
        if (
            manifest_version not in {2, 3}
            or manifest.get("date") != date
            or manifest.get("provider_code") != provider_code
            or manifest.get("max_size_bytes") != max_size
            or not isinstance(manifest.get("batches"), list)
        ):
            raise PackagingError(f"incompatible batch manifest: {output_dir / BATCH_MANIFEST}")
        records = list(manifest["batches"])

    # 在执行任何删除前，先确认清单是现有连续批次的前缀，磁盘批次也没有缺号。
    manifest_names: list[str] = []
    for record in records:
        if not isinstance(record, dict) or not BATCH_FILE_RE.fullmatch(str(record.get("file", ""))):
            raise PackagingError(f"invalid batch record in {output_dir / BATCH_MANIFEST}")
        manifest_names.append(str(record["file"]))
    actual_paths = sorted(
        (
            path
            for path in output_dir.glob("batch_*.tar.gz")
            if BATCH_FILE_RE.fullmatch(path.name)
        ),
        key=lambda path: batch_number(path.name),
    )
    actual_names = [path.name for path in actual_paths]
    expected_names = [f"batch_{number:06d}.tar.gz" for number in range(1, len(actual_names) + 1)]
    if actual_names != expected_names or manifest_names != actual_names[: len(manifest_names)]:
        raise PackagingError(f"batch sequence is missing, reordered, or incompatible in {output_dir}")

    by_file: dict[str, dict[str, object]] = {}
    migrated_records: list[dict[str, object]] = []
    for record in records:
        if not isinstance(record, dict) or not BATCH_FILE_RE.fullmatch(str(record.get("file", ""))):
            raise PackagingError(f"invalid batch record in {output_dir / BATCH_MANIFEST}")
        name = str(record["file"])
        path = output_dir / name
        if name in by_file or not path.is_file() or path.stat().st_size != record.get("size"):
            raise PackagingError(f"missing, duplicate, or changed batch: {path}")
        if record.get("size", 0) >= max_size and record.get("request_count") != 1:
            raise PackagingError(f"multi-request batch exceeds size limit: {path}")
        if manifest_version == 2:
            requests = record.get("requests")
            if not isinstance(requests, list) or len(requests) != record.get("request_count"):
                raise PackagingError(f"invalid request list for v2 batch: {path}")
            names = verify_batch(path, date, requests)
            delete_source_requests(source_dir, names, date)
        clean_record = public_record(record)
        migrated_records.append(clean_record)
        by_file[name] = clean_record
    records = migrated_records

    # 批次先原子发布、清单后更新。若两步之间进程退出，下次只需读取新增批次一次。
    for path in actual_paths:
        if path.name not in by_file:
            record = batch_record(path, date)
            if record["size"] >= max_size and record["request_count"] != 1:
                raise PackagingError(f"multi-request batch exceeds size limit: {path}")
            delete_source_requests(source_dir, record["requests"], date)
            clean_record = public_record(record)
            records.append(clean_record)
            by_file[path.name] = clean_record
            records.sort(key=lambda item: batch_number(str(item["file"])))
            write_manifest(output_dir, date, provider_code, max_size, records)

    records.sort(key=lambda record: batch_number(str(record["file"])))
    for expected, record in enumerate(records, 1):
        if record["file"] != f"batch_{expected:06d}.tar.gz":
            raise PackagingError(f"batch sequence is not contiguous in {output_dir}")
        if record["size"] >= max_size and record["request_count"] != 1:
            raise PackagingError(f"multi-request batch exceeds size limit: {record['file']}")
    if manifest_version == 2:
        write_manifest(output_dir, date, provider_code, max_size, records)
    return records, len(records) + 1


def write_manifest(
    output_dir: Path,
    date: str,
    provider_code: str,
    max_size: int,
    records: Sequence[dict[str, object]],
) -> None:
    """原子更新增量清单。"""

    manifest = {
        "version": 3,
        "date": date,
        "provider_code": provider_code,
        "timezone": "UTC+08:00",
        "max_size_bytes": max_size,
        "request_count": sum(int(record["request_count"]) for record in records),
        "batches": list(records),
    }
    fd, temp_name = tempfile.mkstemp(prefix=".batch-manifest-", dir=output_dir)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as file:
            json.dump(manifest, file, ensure_ascii=False, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        Path(temp_name).replace(output_dir / BATCH_MANIFEST)
        fsync_directory(output_dir)
    finally:
        Path(temp_name).unlink(missing_ok=True)


def archive_directory_time(request: Path) -> dt.datetime:
    """返回请求目录的近似完成时间，不读取或解析目录内的业务文件。"""

    # Linux 上通常无法通过 Python stat 获取可靠的 birth time。请求归档目录在内容
    # 完成后才从 .staging 发布，因此目录 mtime 足以作为“不足半小时”的近似判断。
    return dt.datetime.fromtimestamp(request.stat().st_mtime, tz=UTC_PLUS_8)


def discover_source_dates(archive_root: Path) -> list[str]:
    """发现所有可能含有未打包请求的源日期目录。"""

    if not archive_root.is_dir():
        raise PackagingError(f"request archive root does not exist: {archive_root}")
    dates = [
        entry.name
        for entry in archive_root.iterdir()
        if entry.is_dir() and DATE_DIRECTORY_RE.fullmatch(entry.name)
    ]
    return sorted(dates)


def package_date_incrementally(
    archive_root: Path,
    output_parent: Path,
    provider_code: str,
    date: str,
    cutoff: dt.datetime,
    max_size: int,
) -> tuple[int, int]:
    """把一个日期中达到时间门槛且尚未打包的请求追加为新批次。"""

    source_dir = archive_root / date
    output_dir = output_parent / date
    output_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    records, next_number = load_batch_state(
        output_dir, source_dir, date, provider_code, max_size
    )
    requests = discover_requests(source_dir, date)
    eligible: list[Path] = []
    for request in requests:
        # 成功打包的源目录已经被删除；遇到第一个尚未达到时间门槛的目录就停止。
        # 序号代表请求发生顺序，后续更大序号也留到下一轮，不会越过它先打包。
        if archive_directory_time(request) > cutoff:
            break
        eligible.append(request)
    if not eligible:
        return 0, 0

    staging_root = output_parent / ".packing"
    staging_root.mkdir(mode=0o700, parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=f"{date}-", dir=staging_root))
    try:
        new_records = build_batches(eligible, staging, max_size, next_number)
        for record in new_records:
            name = str(record["file"])
            staged_batch = staging / name
            expected_requests = list(record["requests"])
            verify_batch(staged_batch, date, expected_requests)
            fsync_file(staged_batch)
            target = output_dir / name
            if target.exists():
                raise PackagingError(f"batch target already exists: {target}")
            staged_batch.replace(target)
            fsync_directory(output_dir)
            delete_source_requests(source_dir, expected_requests, date)
            records.append(public_record(record))
            # 每完成一个 batch 的源目录删除就立即登记，缩小中断恢复窗口。
            write_manifest(output_dir, date, provider_code, max_size, records)
    finally:
        shutil.rmtree(staging, ignore_errors=True)
    return len(eligible), len(new_records)


def package_ready_requests(
    archive_root: Path,
    provider_code: str,
    cutoff: dt.datetime,
    max_size: int,
) -> tuple[int, int]:
    """执行一轮扫描；逐日处理，从结构上保证任何批次都不会跨日。"""

    try:
        provider_code = validate_provider_code(provider_code)
    except argparse.ArgumentTypeError as exc:
        raise PackagingError(str(exc)) from exc
    archive_root = archive_root.expanduser().resolve()
    output_parent = MAAS_DATA_ROOT.expanduser().resolve() / "archive" / provider_code
    output_parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    total_requests = total_batches = 0
    for date in discover_source_dates(archive_root):
        request_count, batch_count = package_date_incrementally(
            archive_root, output_parent, provider_code, date, cutoff, max_size
        )
        total_requests += request_count
        total_batches += batch_count
    return total_requests, total_batches


@contextmanager
def process_lock(provider_code: str):
    """持有 provider 级独占锁，阻止两个打包进程同时运行。"""

    lock_dir = MAAS_DATA_ROOT.expanduser().resolve() / "archive" / provider_code
    lock_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock_path = lock_dir / ".packager.lock"
    with lock_path.open("a+") as lock_file:
        try:
            fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise PackagingError(f"another packager instance is already running: {lock_path}") from exc
        yield


def run_scheduler(
    archive_root: Path,
    provider_code: str,
    max_size: int,
    interval_seconds: int,
    min_age_seconds: int,
    once: bool,
) -> None:
    """串行运行内部调度；一轮完成后才开始计算下一轮等待时间。"""

    try:
        provider_code = validate_provider_code(provider_code)
    except argparse.ArgumentTypeError as exc:
        raise PackagingError(str(exc)) from exc
    with process_lock(provider_code):
        while True:
            now = dt.datetime.now(UTC_PLUS_8)
            cutoff = now - dt.timedelta(seconds=min_age_seconds)
            requests, batches = package_ready_requests(
                archive_root, provider_code, cutoff, max_size
            )
            print(
                f"scan complete: cutoff={cutoff.isoformat()} "
                f"requests={requests} batches={batches}",
                flush=True,
            )
            if once:
                return
            time.sleep(interval_seconds)


def build_parser() -> argparse.ArgumentParser:
    """构造独立脚本的命令行参数。"""

    parser = argparse.ArgumentParser(
        description="Continuously package ready request archives into size-bounded tar.gz batches."
    )
    parser.add_argument(
        "--archive-root",
        type=Path,
        default=os.environ.get("REQUEST_ARCHIVE_DIRECTORY") or None,
        required=not os.environ.get("REQUEST_ARCHIVE_DIRECTORY"),
        help="request archive root (env: REQUEST_ARCHIVE_DIRECTORY)",
    )
    parser.add_argument(
        "--provider-code",
        type=validate_provider_code,
        default=os.environ.get("REQUEST_ARCHIVE_PROVIDER_CODE") or None,
        required=not os.environ.get("REQUEST_ARCHIVE_PROVIDER_CODE"),
        help="providerCode output path segment (env: REQUEST_ARCHIVE_PROVIDER_CODE)",
    )
    parser.add_argument(
        "--max-size",
        type=parse_size,
        default=DEFAULT_MAX_SIZE,
        help="compressed-size limit; a single request may exceed it (default: 100MiB)",
    )
    parser.add_argument(
        "--interval",
        type=parse_duration,
        default=parse_duration(os.environ.get("REQUEST_ARCHIVE_PACKAGE_INTERVAL", "30m")),
        help="wait after each completed scan (default/env: 30m / REQUEST_ARCHIVE_PACKAGE_INTERVAL)",
    )
    parser.add_argument(
        "--min-age",
        type=parse_duration,
        default=parse_duration(os.environ.get("REQUEST_ARCHIVE_PACKAGE_MIN_AGE", "30m")),
        help="minimum request age before packaging (default/env: 30m / REQUEST_ARCHIVE_PACKAGE_MIN_AGE)",
    )
    parser.add_argument(
        "--once",
        action="store_true",
        help="run one scan and exit (for maintenance and tests)",
    )
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    """命令行入口：成功返回 0，可预期失败返回 1。"""

    args = build_parser().parse_args(argv)
    try:
        run_scheduler(
            args.archive_root,
            args.provider_code,
            args.max_size,
            args.interval,
            args.min_age,
            args.once,
        )
    except (OSError, PackagingError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
