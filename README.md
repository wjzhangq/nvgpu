# gpumon

单进程的多机指标监控器。按配置文件纳管**本机**和若干台**远程 Linux 机器**，周期性采集 CPU / 内存 / GPU / 显存 / 磁盘，在内存里保留每台机器最近 60 个采样点，通过只读 HTTP JSON 接口对外提供当前值与历史数据。

## 与 gpu2 的区别

| | gpu2 | gpumon |
|---|---|---|
| 架构 | 推模式：每台机器跑 agent，POST 到中心 | 拉模式：中心单进程，远程走无 agent SSH |
| 机器标识 | 随机 UUID（重启即变） | 配置文件里指定的 `name`（稳定） |
| 历史数据 | 无，只存最新一份 | 每机 60 点内存环形缓冲 |
| 部署面 | 每台机器都要装、都要升级 | 只有中心一个二进制 |
| 中心服务 | Python `server.py` | 合进同一个 Go 二进制 |

## 设计要点

**远程采集不装 agent。** 每台远程机维持一条 SSH 长连接，每个采集周期只开一个 session 执行一段 shell，读 `/proc/stat`、`/proc/meminfo`、`df`、`nvidia-smi` 后在中心解析。连接断了下个周期自动重连。

**静态信息只抓一次。** 主机名、机型、CPU 拓扑在建连时抓取并缓存，重连时失效重取。周期脚本只跑动态部分，输出量很小。

**CPU 使用率用跨周期差分算。** `/proc/stat` 是累计值，中心保存上一轮的读数做差，所以远程脚本里不需要 `sleep`，单次采集延迟基本等于一个 RTT。首个样本没有前值，使用率为 0。

**多路 CPU / 多卡 / 多盘。** `cpus` 是数组，按物理 CPU（socket）聚合逻辑核；x86 用 `/proc/cpuinfo` 的 `physical id` / `core id` 分组，aarch64 这些字段不存在，退回 `lscpu`，再退回整机型号。`gpus`、`disks` 同样是数组。

**统一内存（GB10 等）。** 通过「arm64 + 有 GPU + 显存总量与系统内存差距 < 25%」判定，命中时在 `memory.unified` 和 `gpus[].unified` 打标记。**标记只是提示**：`memory` 和 `gpus[].vram_*` 仍各自上报完整数值，不做去重。跨机汇总时是否重复计数由调用方决定。

**离线也写记录。** 采集失败照样往环形缓冲里写一条 `online: false` 的记录，历史序列不会出现时间空洞，前端能画出明确的断点。

**本机 GPU 优先走 NVML。** nvidia-smi 每次调用都要 fork 进程并重新初始化驱动（50-200ms），是单次采集的主要开销。本机采集优先调用 NVML 库（5-15ms），失败时自动回退到 nvidia-smi。远程节点因为无 agent 设计仍走 nvidia-smi。

## 新增功能

**GPU 温度与功耗监控。** 采集 GPU 核心温度（℃）和当前功耗（W）。本机采集通过 NVML 获取（`Device.GetTemperature` / `Device.GetPowerUsage`），远程采集通过 nvidia-smi 的 `temperature.gpu` / `power.draw` 字段。不支持的设备填 0。

**系统服务注册。** 支持将 nvgpu 注册为系统服务，开机自启。Linux 使用 systemd，Windows 使用 Service Control Manager。使用 `nvgpu service install` 命令一键安装。

**内置 Web 看板。** 访问 `http://<listen>/` 可查看实时监控看板，每 5 秒自动刷新。单文件 HTML（约 6KB），编译到二进制，无需额外部署。

## 支持范围

- 中心进程：Linux (amd64 / arm64) 和 Windows (amd64)
- 远程节点：**仅 Linux**（需要 `/proc`、`df`；`lscpu` 可选。`disk_mode: block` 需要 `/sys/block`，`lsblk` 可选）
- 加速卡：**仅 NVIDIA**，本机采集优先使用 NVML 库（快 4-5 倍），远程采集使用 `nvidia-smi`
- 最多一个 `type: local` 节点，远程节点数量不限

## 采集性能

单次采集的耗时分布（`collect_ms` 字段可以直接观察）：

| 指标 | 耗时 | 说明 |
|---|---|---|
| CPU + 内存 | 2-3ms | 读 `/proc`，已无优化空间 |
| 磁盘（首次/缓存过期）| 5-20ms | 扫挂载点 + 查用量 |
| 磁盘（缓存命中）| 1-2ms | 复用挂载点列表，只查用量 |
| GPU（NVML）| 5-15ms | 直接调库（含温度/功耗采集） |
| GPU（nvidia-smi）| 50-200ms | fork 进程 + 重新初始化驱动 |

**有 GPU 的本机节点**：默认构建约 20ms，`-tags no_nvml` 构建约 90ms。
**无 GPU 的节点**：约 4-10ms，取决于挂载点缓存是否命中。
**远程节点**：延迟基本等于 GPU 采集耗时加一个 RTT，远程始终走 nvidia-smi。

两处优化点：

**GPU 走 NVML。** nvidia-smi 的开销几乎全在进程启动和驱动初始化上，查询本身很快。NVML 是 nvidia-smi 内部调用的同一个库，初始化一次后复用，省掉这两笔开销。本机采集优先走 NVML，初始化失败自动回退，不需要配置。

**挂载点列表带缓存。** `disk.Partitions()` 每次都要扫 `/proc/mounts`（Windows 是注册表），但挂载点变化是分钟级以上的事。默认缓存 60s，只有用量查询每周期都做。用 `partition_cache_ttl` 调整，设成 `0s` 关掉。代价是新挂载的盘最多晚 60s 出现。

| 方式 | 本机延迟 | 适用场景 |
|------|---------|---------|
| NVML 库（默认）| 5-15ms | 本机采集，需要 CGO 和 NVIDIA 驱动 |
| nvidia-smi | 50-200ms | 远程 SSH 采集，或 `-tags no_nvml` 构建 |

## 构建

### 默认构建（启用 NVML，推荐）

默认启用 NVML 库支持，GPU 采集速度提升 **4-5 倍**（5-15ms vs 50-200ms）。需要 CGO 和 gcc/g++。

```bash
go mod tidy
go build -o nvgpu .
```

**运行时依赖**：
- Linux: `libnvidia-ml.so.1`（随 NVIDIA 驱动安装，通常在 `/usr/lib64` 或 `/usr/lib/x86_64-linux-gnu`）
- Windows: `nvml.dll`（随驱动安装在 `C:\Program Files\NVIDIA Corporation\NVSMI\`）
- macOS: 不支持（无 NVIDIA GPU）

NVML 初始化失败时自动回退到 nvidia-smi，无需手动切换。

### 纯 Go 构建（禁用 NVML）

不依赖 CGO，适合交叉编译或无 gcc 环境，GPU 采集使用 nvidia-smi。

```bash
go build -tags no_nvml -o nvgpu .

# 交叉编译
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -tags no_nvml -ldflags "-s -w" -o nvgpu-linux-amd64
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -tags no_nvml -ldflags "-s -w" -o nvgpu-linux-arm64   # GB10 / Grace
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags no_nvml -ldflags "-s -w" -o nvgpu.exe
```

## 运行

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml
./nvgpu -config config.yaml
```

### 命令行参数

**常规运行**：

| 参数 | 说明 |
|---|---|
| `-config` | 配置文件路径，默认 `config.yaml` |
| `-listen` | 覆盖配置里的 `server.listen` |
| `-version` | 打印版本号后退出 |

**GPU 诊断**：

```bash
nvgpu gpu-probe
```

打印 GPU 采集诊断报告：nvidia-smi 探测结果、实际执行的命令、退出状态、原始输出、解析结果。

**适用场景**：
- Windows 服务里 GPU 数据是空的，不知道为什么
- 检查 nvidia-smi 是否被正确探测（自动探测结果或配置的路径）
- 验证输出格式能否被解析

诊断报告可直接复制发给项目维护者，便于远程定位。

**服务管理**：

```bash
nvgpu -config <path> service <command>
```

| 命令 | 说明 |
|---|---|
| `install` | 安装系统服务（需要管理员权限） |
| `uninstall` | 卸载系统服务 |
| `start` | 启动服务 |
| `stop` | 停止服务 |
| `restart` | 重启服务 |
| `status` | 查看服务状态 |

**注意**：
- `-config` 参数必须在 `service` 之前
- 建议使用绝对路径（systemd 工作目录可能不同）
- SSH 私钥路径也需要绝对路径

## 配置

配置文件按**严格模式**解析：字段名拼错会直接报错退出，不会被静默忽略。完整示例见 `config.example.yaml`。

```yaml
server:
  listen: "0.0.0.0:8080"
  cors: true

defaults:
  interval: 5s          # 全局默认采集间隔
  history_size: 60      # 每机历史点数，硬上限 60
  collect_timeout: 10s  # 实际生效 = min(collect_timeout, 该节点 interval)

nodes:
  - name: px-workstation
    type: local
    interval: 2s

  - name: spark-01
    type: ssh
    interval: 5s
    disks: ["/", "/data"]
    ssh:
      host: 192.168.1.11
      user: nvidia
      key_file: ~/.ssh/id_ed25519
      known_hosts_file: ~/.ssh/known_hosts
```

### 节点字段

| 字段 | 说明 |
|---|---|
| `name` | 唯一标识，出现在所有 API 响应里 |
| `type` | `local` 或 `ssh`；留空时按有无 `ssh` 段推断 |
| `interval` | 覆盖 `defaults.interval`，最小 500ms |
| `disk_mode` | 磁盘采集模式：`mount`（挂载点，默认）或 `block`（物理磁盘），见下文"磁盘采集模式"章节 |
| `disks` | 白名单，留空自动发现。`mount` 模式：挂载点列表如 `["/", "/data"]`（Windows: `["C:\\"]`）；`block` 模式：设备名如 `["nvme0n1", "sda"]`，支持带 `/dev/` 前缀（Windows: `["0", "1"]`） |
| `nvidia_smi` | `nvidia-smi` 路径，留空自动探测 |
| `partition_cache_ttl` | 挂载点缓存 TTL（仅 `type: local` 且 `disk_mode: mount` 时有效）。挂载点变化频率极低，缓存可节省 5-20ms。留空默认 60s，显式设为 `0s` 禁用 |

### SSH 认证

三选一或组合（按 key → agent → password 顺序尝试）：

```yaml
ssh:
  key_file: ~/.ssh/id_ed25519
  passphrase: ""              # 私钥带口令时填
# 或
  use_agent: true             # 需要中心进程环境里有 SSH_AUTH_SOCK
# 或
  password: "..."
```

主机指纹默认校验，读 `~/.ssh/known_hosts`。**首次连接前先手动 `ssh` 一次把指纹录进去**，否则会报错。隔离实验网可以用 `insecure_skip_host_key_check: true` 跳过。

### 磁盘采集模式

`disk_mode` 控制磁盘采集的语义，有两种模式。

#### `mount` 模式（默认）

按**挂载点**采集文件系统容量，历史行为，向后兼容。

- 报告已挂载文件系统的容量与用量
- `disks` 白名单写挂载点，如 `["/", "/data"]`（Windows 写盘符 `["C:\\"]`）
- 自动发现时排除伪文件系统（tmpfs / overlay / proc 等），忽略 <1 GiB 的小分区
- 局限：
  - 报的是文件系统可用容量，不是磁盘原始容量。ext4 默认给 root 预留 5%，2 TB 的盘 `df` 只看到 ~1.9 TB
  - 未挂载的分区和磁盘完全不可见
  - 一块物理盘上的多个分区会产生多条记录

#### `block` 模式

按**物理块设备**采集磁盘原始容量，不关心挂载目录。

- 报告物理磁盘的原始容量（`/sys/block/<dev>/size` × 512）、型号、`rotational` 标记
- `disks` 白名单写设备名，如 `["nvme0n1", "sda"]`，支持带 `/dev/` 前缀（Windows 写磁盘序号 `["0", "1"]`）
- 自动发现时排除虚拟设备（loop / dm / md / zram 等）和光驱，忽略 <10 GiB 的小盘
- 远程节点优先用 `lsblk`，缺了就退回纯 `/sys/block` 的 shell 循环，**不需要 root，也不需要额外二进制**
- **限制：`used_bytes` 与 `usage_percent` 恒为 0。** 块设备层面没有"已用"概念，要算用量得聚合它所有分区的挂载点，当前版本不做
- 适用场景：回答"这台机器有几块物理盘、每块多大、什么型号"

示例：

```yaml
nodes:
  - name: file-server
    type: ssh
    disk_mode: mount          # 默认，可省略
    disks: ["/", "/data"]     # 挂载点白名单
    
  - name: storage-server
    type: ssh
    disk_mode: block
    disks: ["nvme0n1", "sda", "sdb"]  # 物理磁盘白名单
```

### 常见坑

**远程 SSH 采集找不到 nvidia-smi**：非交互式 SSH 会话拿到的 `PATH` 通常很窄，`nvidia-smi` 经常不在里面。脚本已经自动补了几个常见目录，仍然找不到的话在节点上写死 `nvidia_smi: /usr/bin/nvidia-smi`。

**Windows 服务里 GPU 数据是空的**：
1. 用管理员权限运行 `nvgpu.exe gpu-probe`，查看诊断报告
2. 如果报告显示"未找到"，手动在 PowerShell 里执行 `where.exe nvidia-smi.exe`，把完整路径填进配置：
   ```yaml
   nodes:
     - name: local
       type: local
       nvidia_smi: "C:\\Windows\\System32\\DriverStore\\FileRepository\\nvdm...\\nvidia-smi.exe"
   ```
3. 重启服务：`nvgpu.exe service restart`


## HTTP API

所有接口只读，只接受 `GET`。**没有鉴权**，请只绑内网地址或放在反向代理后面。

### `GET /`

**Web 看板**（v1.1+ 新增）。返回单页面 HTML 监控看板，展示所有节点的实时指标，每 5 秒自动刷新。

浏览器访问 `http://<listen>/` 即可查看。

### `GET /api/v1/metrics`

当前指标。`?node=a,b` 过滤（可重复传参），留空返回全部。

```json
{
  "generated_at": "2026-08-16T10:00:00+08:00",
  "nodes": [
    {
      "node": "spark-01",
      "timestamp": "2026-08-16T10:00:00+08:00",
      "online": true,
      "collect_ms": 42,
      "host": {
        "hostname": "spark-01",
        "os": "linux",
        "platform": "Ubuntu 24.04.1 LTS",
        "kernel": "6.11.0-1004-nvidia",
        "arch": "aarch64",
        "model": "NVIDIA DGX Spark"
      },
      "cpus": [
        {
          "index": 0,
          "model": "Cortex-A725",
          "physical_cores": 20,
          "logical_cores": 20,
          "usage_percent": 12.35
        }
      ],
      "memory": {
        "total_bytes": 128849018880,
        "used_bytes": 34359738368,
        "available_bytes": 94489280512,
        "usage_percent": 26.67,
        "unified": true
      },
      "gpus": [
        {
          "index": 0,
          "model": "NVIDIA GB10",
          "uuid": "GPU-...",
          "utilization_percent": 87.0,
          "vram_total_bytes": 127928205312,
          "vram_used_bytes": 68719476736,
          "vram_usage_percent": 53.72,
          "temperature_celsius": 72,
          "power_watts": 235.6,
          "unified": true
        }
      ],
      "disks": [
        {
          "mount": "/",
          "device": "/dev/nvme0n1p2",
          "fstype": "ext4",
          "total_bytes": 4000787030016,
          "used_bytes": 1200236122112,
          "usage_percent": 30.0
        }
      ]
    }
  ]
}
```

`mount` 模式示例（上方已展示），`block` 模式示例：

```json
{
  "node": "storage-server",
  "disks": [
    {
      "device": "nvme0n1",
      "total_bytes": 2000398934016,
      "used_bytes": 0,
      "usage_percent": 0,
      "type": "disk",
      "model": "Samsung SSD 980 PRO 2TB",
      "rotational": false
    },
    {
      "device": "sda",
      "total_bytes": 4000787030016,
      "used_bytes": 0,
      "usage_percent": 0,
      "type": "disk",
      "model": "WDC WD40EZRZ-00WN9B0",
      "rotational": true
    }
  ]
}
```

**block 模式特征**：`mount` 字段为空或不存在，`type` / `model` / `rotational` 有值，`used_bytes` 与 `usage_percent` 恒为 0。

节点离线时 `online` 为 `false` 并带 `error` 字段，`host` 保留上次已知的静态信息。

### `GET /api/v1/history`

历史数据，每个节点最多 60 个采样点，**按时间从旧到新**排列。

参数：`?node=a,b`（留空全部）、`&limit=N`（默认取满，超过窗口大小按窗口截断）。

```json
{
  "generated_at": "...",
  "history_size": 60,
  "limit": 60,
  "nodes": [
    { "node": "spark-01", "count": 60, "samples": [ /* Snapshot 数组 */ ] }
  ]
}
```

### `GET /api/v1/nodes`

节点清单与在线状态，不含指标明细。

```json
{
  "generated_at": "...",
  "nodes": [
    {
      "name": "spark-01",
      "type": "ssh",
      "target": "nvidia@192.168.1.11:22",
      "interval_seconds": 5,
      "online": true,
      "last_seen": "...",
      "hostname": "spark-01"
    }
  ]
}
```

### `GET /healthz`

存活探针。

## GPU 温度与功耗字段

v1.1+ 新增：

- `temperature_celsius`：GPU 核心温度（摄氏度），0 表示不支持或未采集
- `power_watts`：当前功耗（瓦特），0 表示不支持或未采集

本机采集通过 NVML API 获取，远程采集通过 nvidia-smi 的 `temperature.gpu` 和 `power.draw` 字段。

## 单位约定

容量类字段一律 **字节**，百分比为 0–100 的浮点数（两位小数）。服务端不做 GB 换算，避免有损取整——客户端自己除。

## 历史数据的语义

- 纯内存环形缓冲，**进程重启即清空**，不落盘。
- 每个节点独立一个缓冲，容量 `history_size`（上限 60）。
- 缓冲覆盖的时间跨度 = `history_size × 该节点的 interval`。`interval: 5s` 就是 5 分钟。
- 不同节点可以有不同的 `interval`，所以各自历史序列的时间粒度可能不同，前端做叠加图时要注意对齐。

## 部署

### 作为系统服务运行（推荐）

**Linux**：
```bash
# 1. 准备配置文件（使用绝对路径）
sudo mkdir -p /opt/nvgpu
sudo cp nvgpu /opt/nvgpu/
sudo cp config.yaml /opt/nvgpu/

# 2. 安装服务
sudo /opt/nvgpu/nvgpu -config /opt/nvgpu/config.yaml service install

# 3. 启动服务
sudo systemctl start nvgpu

# 4. 查看状态
sudo systemctl status nvgpu
journalctl -u nvgpu -f

# 5. 开机自启
sudo systemctl enable nvgpu
```

生成的 systemd unit 位于 `/etc/systemd/system/nvgpu.service`。

**Windows**：
```cmd
REM 以管理员身份运行 PowerShell

REM 1. 准备配置文件
mkdir C:\nvgpu
copy nvgpu.exe C:\nvgpu\
copy config.yaml C:\nvgpu\

REM 2. 安装服务
C:\nvgpu\nvgpu.exe -config C:\nvgpu\config.yaml service install

REM 3. 启动服务
sc start nvgpu

REM 4. 查看状态
sc query nvgpu
```

**卸载**：
```bash
# Linux
sudo systemctl stop nvgpu
sudo /opt/nvgpu/nvgpu service uninstall

# Windows（管理员 PowerShell）
sc stop nvgpu
C:\nvgpu\nvgpu.exe service uninstall
```

**注意**：systemd 拉起的进程环境很干净，用 `use_agent` 认证的话 `SSH_AUTH_SOCK` 不会存在，用 `~` 路径的话 `HOME` 需要能解析。生产环境建议用绝对路径 + `key_file`。

### 手动运行（开发/测试）

```bash
./nvgpu -config config.yaml
```

按 Ctrl+C 退出。

## 后续可加的东西

按当前需求都刻意没做，需要的话都是小改动：

- 跨机汇总接口（对应 gpu2 的 `/merge`）
- Prometheus `/metrics` 导出
- API Token 鉴权
- 磁盘 IO、网络吞吐监控
- GPU 风扇转速、频率监控
- 多传感器温度（显存温度、PCB 温度）
- AMD / 国产 NPU 支持（采集器接口已经抽象好，加一个实现即可）
