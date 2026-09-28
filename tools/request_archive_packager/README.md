# 请求归档增量分包任务

脚本独立于 sub2api 服务运行，只依赖 Python 3 标准库。它是一个长驻、串行的内部
调度器，不依赖 cron 或 systemd timer。默认每轮结束后等待 30 分钟，再扫描所有
目录时间早于当前时间 30 分钟、且尚未打包的正式请求归档。

## 目录结构

原始请求从配置目录的日期子目录读取：

```text
${REQUEST_ARCHIVE_DIRECTORY}/2026-09-23/req_20260923_000001/...
```

压缩文件固定发布到：

```text
/maasData/archive/${REQUEST_ARCHIVE_PROVIDER_CODE}/${yyyy-MM-dd}/
```

例如：

```text
/maasData/archive/provider-a/2026-09-23/batch_000001.tar.gz
/maasData/archive/provider-a/2026-09-23/batch_000002.tar.gz
/maasData/archive/provider-a/2026-09-23/batch_manifest.json
```

脚本逐个日期处理并分别维护批次号，因此日切前后的请求绝不会进入同一个压缩包。
每个包中的顶层目录仍是 `req_YYYYMMDD_NNNNNN/`，其中的三个业务文件保持原结构。

## 调度与时间门槛

默认配置：

```bash
REQUEST_ARCHIVE_PACKAGE_INTERVAL=30m
REQUEST_ARCHIVE_PACKAGE_MIN_AGE=30m
```

- `INTERVAL`：一轮扫描和打包全部结束后，再等待多久启动下一轮。
- `MIN_AGE`：请求归档目录至少存在多久后才允许打包。筛选只读取目录的 `mtime`，
  不提前读取或解析 `metadata.json`。

由于等待从上一轮完成后才开始计算，即使某轮运行超过 30 分钟，也不会与下一轮
重叠。脚本还会持有 provider 级文件锁；误启动第二个实例时，第二个实例会立即失败。

每轮会列出日期目录中的请求目录名并按请求序号升序遍历，遇到第一个距当前不足
`MIN_AGE` 的目录就停止该日期扫描，不再检查后续目录。业务文件只在真正写入
tar.gz 时读取。新批次从该日期已有的最大批次号继续编号。

每个 batch 完整关闭后，会重新打开并完整读取一次，校验 gzip/tar、成员路径、日期
和请求目录集合。校验成功后依次执行：持久化临时包、原子发布、持久化目标目录、
逐个删除该包对应的原请求目录、原子更新 `batch_manifest.json`。任一步失败都会停止
当天后续处理，不会在压缩包未可靠发布时删除源数据。

如果进程在“压缩包发布”和“清单更新”之间退出，下次启动会完整校验这个未登记
batch，从包内恢复准确请求目录，继续删除尚存的原目录，最后补写清单。

## 启动

可通过环境变量配置：

```bash
export REQUEST_ARCHIVE_DIRECTORY=/app/data/request-archives
export REQUEST_ARCHIVE_PROVIDER_CODE=provider-a
export REQUEST_ARCHIVE_PACKAGE_INTERVAL=30m
export REQUEST_ARCHIVE_PACKAGE_MIN_AGE=30m
python3 package_request_archives.py
```

也可使用命令行：

```bash
python3 package_request_archives.py \
  --archive-root /app/data/request-archives \
  --provider-code provider-a \
  --interval 30m \
  --min-age 30m \
  --max-size 100M
```

`--once` 只执行一轮后退出，适合人工维护和验证，不用于周期调度。

## systemd 长驻服务

不要配置 cron 或 systemd timer。可将本目录中的
`request-archive-packager.service` 安装为普通长驻服务，由 systemd 只负责启动、
故障重启和开机拉起：

```bash
sudo cp request-archive-packager.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now request-archive-packager.service
```

示例 unit 假设代码位于 `/opt/sub2api`、配置位于 `/opt/sub2api/.env`，并由
`sub2api` 用户运行。部署前应按实际用户和安装路径调整。

## 100 MiB 分包逻辑

脚本不按请求数量或源文件大小估算。它按请求序号逐个写入 tar.gz，并观察实际压缩
字节数。接近 96 MiB 后停止追加新请求，关闭 tar/gzip 后再检查最终文件：

- 多请求包 `< 100 MiB` 时发布。
- 多请求包 `>= 100 MiB` 时移除最后一个请求并重压，直到低于上限。
- 单个不可拆分请求自身 `>= 100 MiB` 时，允许独占一个包发布。

## 清单格式

`batch_manifest.json` 使用 v3 精简格式。每个批次只记录：

```text
file、size、sha256、request_count、first_request、last_request
```

不再保存 `requests` 列表。`first_request` 和 `last_request` 只供查看范围，准确内容
始终以 tar.gz 内实际目录为准。正常情况下，源请求目录是否仍存在就是待处理状态。

脚本只删除已经完整校验并可靠发布到 batch 的 `req_*` 目录，不删除原日期目录。

## 测试

```bash
cd tools/request_archive_packager
python3 -m unittest -v
```
