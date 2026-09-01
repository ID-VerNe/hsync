# hsync — hardlink 目录镜像 CLI（规划）

状态：待批准（v2，已并入用户四项决策 + 两个新增防线）
日期：2026-09-01
目标目录：`C:\Users\VerNe\Downloads\Documents\hsync`（当前为空）

---

## 0. 需求澄清与语义约定（已与用户对齐）

1. **单向镜像，不是双向**：source 是唯一真理，target 是它的镜像。hardlink 下内容天然共享，不存在"两侧各自的内容"。
2. **断链重链（P0，已确认）**：AI Agent 原子保存（写临时 + rename）会换掉 source 文件 inode，target 同名文件静默断链成旧内容副本。sync 对**每一层树中**的同名文件做 fileID（inode）比对：相同 → 健康跳过；不同 → `Remove(target)` + 重新 `Link`（source 真理，不做 target-wins 吸收）。
3. **"不校验内容"限定为"不读文件内容"**：inode/大小等元数据比对仍做。
4. **递归镜像（新确认）**：不限于第一层。source 子目录 → target 用 `MkdirAll` 建立**真实物理文件夹**（目录不可以用硬链/软链代替），再递归进入同步。target 侧 source 已无的**僵尸目录** → 递归清理。
5. **退出码（已确认）**：有跳过项仍 **exit 0**，非致命警告全走 stderr，绝不因个别文件占用阻断 Agent 后续工作。用法错误/registry 损坏才 exit 1/2。
6. **命名**：`hsync`，无冲突。

### 黑名单跳过（新确认，防爆炸）

遍历时（source 与 target 两侧**同时**生效），遇到以下项**一律跳过，不进入、不镜像、也不删除**：
- 任何以 `.` 开头的文件或文件夹（`.git`、`.DS_Store`、`.env`、`.venv`、`.gitignore` 等点文件也不镜像）；
- 全等 `node_modules` 的文件夹。

后果确认：target 里 source 已消失的 dot 目录 / node_modules 也**不会**被清理（保留用户数据）；点文件不会被镜像。

### 僵尸目录清理语义（需确认）

target 侧出现"该名字在 source 完全不存在"的目录 → 递归 `os.RemoveAll` 整棵子树。按镜像模型这是正确的（source 已删该目录，target 内是其最后硬链引用），但 **target 中任何 source 从未有过的用户手动内容会被一并删除**。已确认按此执行。

---

## 1. 调研结论（glue-engineer 真实数据）

工具链已确认存在：`go 1.26.1`、`rust 1.97.0`（本机已装）。

**没有现成可复用的工具**。`polyglot discover` 对 "hardlink directory sync mirror" 类查询返回 0 个有效仓库；扫到的相关项目各不匹配：

| 项目 | 语言 | 定位 | 为什么不适用 |
|---|---|---|---|
| `yukimemi/yui` (yui-cli) | Rust | dotfiles 管理器，hardlink/junction/symlink，含断链分类器（InSync/RelinkOnly/AutoAbsorb） | 文件级映射 + 模板 + 吸收策略，命令模型与状态文件与需求不符。**仅借鉴 RelinkOnly 思路（已纳入 P0）** |
| `likun7981/hlink` | Node | NAS 媒体库批量硬链去重 | 去重非镜像，Node 生态不适合单文件极速 hook |
| `chadnetzer/hardlinkable` (Go) | Go | 目录内内容相同文件硬链去重 | 无 registry 无镜像 |
| `kornelski/dupe-krill` (Rust) | Rust | 增量去重 | 去重场景 |

结论：**自研**。yui 的断链重链语义是唯一值得借鉴的点，已纳入。

---

## 2. 语言选型：Go（已定）

1. **标准库零依赖覆盖全部需求**：`os.Link` / `os.Remove` / `os.RemoveAll` / `os.MkdirAll` / `os.ReadDir` / `os.SameFile` / `encoding/json` / `os.Rename`(原子写 registry)。零第三方依赖。
2. **goroutine + channel 天然表达有界并发**。
3. **Windows NTFS hardlink 一等公民**，`os.SameFile` 基于 VolumeSerial + FileIndex 官方实现。
4. Rust 无优势（无复杂内存/性能结构，`tokio` 对系统调用密集型任务更绕）。

不引入 cobra/clap 等 CLI 框架（4 子命令手动分派最简）。

---

## 3. 架构设计

### 3.1 全局 registry

- 路径：`~/.hsync/registry.json`，`$HSYNC_HOME` 可覆盖。
- 格式（version 字段留演进位）：

```json
{
  "version": 1,
  "entries": [
    { "id": "8f3a...", "source": "C:\\abs\\src", "target": "C:\\abs\\dest" }
  ]
}
```

- id：`source|target` 规范化后 SHA-256 前 8 位 hex（确定性，重复 add 报错已有映射）。
- **写：原子替换**（临时文件 + `os.Rename`）。并发 sync 进程读不到半个文件。
- **并发模型：sync 只读 registry 绝不写**（hook 高频触发）；add/remove 低频才写，进程内锁 + 原子替换。跨进程 add/remove 竞争丢更新的理论窗口：可接受（低频人工操作），不做文件锁——已知限制。

### 3.2 命令

| 命令 | 行为 |
|---|---|
| `hsync add <src> <dest>` | abs 规范化 → 校验 src 存在且为目录、src/dest 不同一目录 → 写 registry → 同步执行一次该 pair 的 sync |
| `hsync remove <id\|src>` | 按 id 或 source 绝对路径匹配删除；无匹配报错退出 1 |
| `hsync list` | 打印 `id  source -> target` |
| `hsync sync` | 读取全部 entries，有界并发递归对齐全部 pair |

退出码：用法错误/registry 损坏 exit 1/2；有跳过项 exit 0。

### 3.3 sync 算法（递归树镜像，核心）

对每个 pair 递归调用 `syncTree(srcDir, dstDir)`，**只镜像普通文件与目录，不处理符号链接**：

1. `os.ReadDir(srcDir)` 过滤黑名单 → 集合 A；`os.ReadDir(dstDir)` 过滤黑名单 → 集合 B。（`ReadDir` 开读即关句柄，不随递归深度堆积句柄）
2. **diff B − A**（target 有、source 无）：
   - 目录 → `os.RemoveAll(dst/name)`（僵尸目录清理，见 0 节语义）；
   - 普通文件 → `os.Remove(dst/name)`（孤儿文件清理）；
   - 符号链接 → 跳过 + warning（非本工具创建，不删）。
3. **diff A − B**（source 有、target 无）：
   - 目录 → `os.MkdirAll(dst/name)` 建真实物理目录，然后递归 `syncTree(src/name, dst/name)`；
   - 普通文件 → `os.Link(src/name, dst/name)`（hardlink，不拷贝内容）；
   - 符号链接 → 跳过。
4. **同名交集 A ∩ B**：
   - 两侧都是目录 → 递归 `syncTree`；
   - 两侧都是普通文件 → `os.SameFile` 比对：
     - 相同 → 健康硬链，跳过；
     - 不同 → 断链 → `os.Remove(dst/name)` + 重新 `os.Link`（P0 重链，source 真理）；
   - 一侧文件一侧目录（冲突）→ 跳过 + warning。

每层遍历用 `DirEntry.Type()` 判断类型（免逐项 lstat）；只有同名文件做 `SameFile` 时 `Info()` 取一次 FileInfo。全部操作 = 目录枚举 + inode 元数据 + link/remove 系统调用，**零内容 IO、零哈希**。

### 3.4 性能与有界并发

- **多 pair 有界并发**：每 pair 一个 goroutine，但全局用 `buffered channel` 作为信号量（`maxConcurrency = 16`，命名常量）限制同时执行 IO 的 pair 数，防止 NTFS 上百 pair 瞬间爆发撞文件句柄上限或触发 Defender 高频 IO 拦截。
- **单 pair 内部串行**：树形递归天然分层，串行最简单且每层仅持有 ~1 个目录句柄；pair 内再并发只会增加锁与上下文切换成本。
- **黑名单**保证 `node_modules`/`.git`/`.venv` 等不入遍历，毫秒级保持成立。
- **不 watch、不常驻**：每次 sync 独立进程，做完即退。

### 3.5 容错清单（全部 warning + 跳过 + exit 0，不 crash）

| 场景 | 处理 |
|---|---|
| target 目录不存在 | warning，跳过该 pair（add 时例外：显式 MkdirAll 后立即同步） |
| 文件被锁定 / Remove/Link 权限错误 | warning，跳过该项（幂等，下次 sync 重试） |
| source 不存在 / 不是目录 / 是符号链接 | warning，跳过该 pair |
| source 与 target 同一目录 | add 拒绝；sync warning 跳过 |
| 同名文件/目录冲突 | warning，跳过该项 |
| 跨卷 hardlink（EXDEV） | warning，跳过该项 |
| registry 损坏 / JSON 解析失败 | 报错退出 1，**不静默重建** |
| 单项 stat 时文件刚消失 | warning，跳过该项 |

---

## 4. 明确不做

- 内容哈希校验、watch 常驻、模板/吸收策略（yui 那套）、双向合并、符号链接镜像。
- CLI 框架、配置文件、dry-run、日志文件。
- `__pycache__` 等非点前缀缓存目录的跳过（如需可加，当前仅按确认的两条黑名单规则）。

---

## 5. 实施步骤

1. 初始化 `go.mod`（module hsync，go 1.26）。
2. 单文件 `main.go`（~400–500 行）：registry 读写（原子替换）、4 子命令、递归 `syncTree`、黑名单过滤、有界并发、容错。
3. 本机验证（Windows/NTFS）：
   - smoke：`add` 后断言 `dst/X` 存在且 `os.SameFile(src/X, dst/X)`；
   - 增/删：src 增删文件与子目录 → `sync` → target 对应增/删（含多层嵌套如 `skills/utils/api.js`）；
   - 僵尸目录：删 src 子目录 → `sync` → target 对应树被 RemoveAll；
   - 断链重链：`Remove(dst/X)` 后新写 `dst/X`（模拟原子保存）→ `sync` → 重新硬链回 source；
   - 黑名单：src 放 `.git/`、`node_modules/` → `sync` → target 无对应项且遍历时间不劣化；
   - 容错：target 指向不存在路径 → 仅 warning，exit 0；
   - 计时：1k+ 文件多 pair 的 sync 耗时确认毫秒级。
4. 安装到 `C:\Users\VerNe\.local\bin\hsync.exe`（`go build -o`）。

---

## 6. 交付物

- `.plan/hsync.md`（本文件）
- `go.mod` + `main.go`
- `C:\Users\VerNe\.local\bin\hsync.exe`
- 验证记录

---

## 7. 决策记录（v2 全部已定）

| 决策点 | 结论 |
|---|---|
| 断链重链 | P0 必做（已确认） |
| 有跳过项退出码 | exit 0，警告走 stderr（已确认） |
| 递归镜像 | 必须递归，目录 MkdirAll 物理建立，僵尸目录递归清理（已确认） |
| 黑名单 | `.` 前缀 + `node_modules` 一律跳过，两侧遍历均生效（已确认） |
| 有界并发 | buffered channel 信号量，上限 16（已确认） |
| 僵尸清理 = RemoveAll 数据删除 | 已确认（见 0 节语义） |
| 点文件不镜像 | 已确认 |
