# hsync

基于**硬链接（Hardlink）**的目录镜像同步工具。source 目录是唯一真理，target 目录是它的镜像：文件通过硬链共享同一个 inode，内容改动天然同步，`sync` 只处理文件/目录的**增、删、断链重链**，从不拷贝文件内容。

## 核心场景

AI Agent 常常受限于工具安全策略，无法直接写某个受保护目录。硬链接可以绕过这类限制：把 Agent 的源目录镜像到一个 Agent 可写的 target，两边共享同一份数据。但硬链接无法自动同步"新增 / 删除 / 原子保存替换 inode"，所以需要一个按需触发的快速同步命令。

`hsync` 的设计就是为此服务的：**在 Agent 每次 `write/create/delete` 之后，由 Hook 触发一次 `hsync sync`**。它只读全局 registry、毫秒级完成、出错时静默跳过（exit 0），不会阻断 Agent 的工作流。

## 特性

- **单向镜像**：source 为唯一真理，target 跟随 source，不做双向合并。
- **递归同步**：子目录在 target 侧建立**真实物理目录**后递归进入；target 侧 source 已删除的僵尸目录被递归清理。
- **断链重链**：AI Agent 的原子保存（写临时文件 + rename 覆盖）会换掉 source 文件的 inode，导致 target 同名文件静默断链成旧副本。`sync` 对同名文件做 **inode 比对**（`os.SameFile`），发现不同即 `Remove` + 重新硬链，target 重新跟上 source。
- **零内容 IO**：全程只做目录枚举 + 元数据比对 + link/remove 系统调用，不读文件内容、不算哈希。
- **黑名单跳过**：任何以 `.` 开头（`.git`、`.env`、`.venv`、`.DS_Store`…）或名为 `node_modules` 的项，在遍历两侧都**不进入、不镜像、不删除**，防止毫秒级退化成分钟级。
- **worker 池并发**：固定 16 个 worker 从任务队列拉取 pair 任务，限制同时进行文件系统 IO 的数量，避免 NTFS 上百对目录瞬间爆发撞句柄上限或触发 Defender 拦截。
- **全局 registry**：映射存于 `~/.hsync/registry.json`（`$HSYNC_HOME` 可覆盖），原子替换写入，`sync` 只读不写，天然支持 Hook 高并发调用。
- **容错优先**：target 缺失、文件锁定、跨卷硬链（EXDEV）、同名文件/目录冲突等一律 warning 跳过并继续，进程仍 exit 0。

## 安装

需要 Go 1.26+：

```bash
cd hsync
go build -o "$HOME/.local/bin/hsync.exe" .
# Windows 下将 $HOME/.local/bin 加入 PATH 即可全局调用
```

## 快速开始

```bash
# 注册一对目录并立即同步（source 为基准）
hsync add C:\workspace\agent-src C:\workspace\agent-mirror

# 查看所有托管映射
hsync list

# 手动触发一次全量同步（推荐挂在 Agent Hook 上）
hsync sync

# 移除映射（按 id 或 source 路径）
hsync remove <id|src>
```

示例输出：

```
$ hsync add D:\proj\src D:\proj\mirror
hsync: added 3e265cae (D:\proj\src -> D:\proj\mirror)

$ hsync sync
hsync: linked=12 relink=1 removed=3 issues=0
```

`linked` 新建硬链数、`relink` 断链重链数、`removed` 清理孤儿数、`issues` 容错跳过项数。

## 工作原理

### 同步算法（递归树镜像）

对每个 pair 递归 `syncTree(srcDir, dstDir)`，只处理普通文件与目录（符号链接不镜像）：

1. 读取两侧目录项，过滤黑名单，得到集合 A（source）、B（target）。
2. **B − A**（target 有、source 无）：文件 `Remove`，目录递归 `RemoveAll`，符号链接跳过。
3. **A − B**（source 有、target 无）：目录 `MkdirAll` 建物理目录后递归；普通文件 `os.Link` 硬链。
4. **A ∩ B**（同名交集）：
   - 两侧都是目录 → 递归；
   - 两侧都是普通文件 → inode 比对：相同则健康跳过，不同则 `Remove` + 重新 `os.Link`（断链重链，source 真理）；
   - 一侧文件一侧目录 → 跳过 + warning。

### 与 AI Agent Hook 集成

以 Claude Code 为例，在 `settings.json` 中为 `Write` / `Edit` / `Create` / `Delete` 等工具配置 hook，工具执行完毕后触发：

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write|Edit|Create|Delete",
        "hooks": [{ "type": "command", "command": "hsync sync" }]
      }
    ]
  }
}
```

`hsync sync` 完成后输出统计到 stdout，所有警告走 stderr，有跳过项仍 exit 0 —— Hook 不会因此误报失败。

## 退出码

| 退出码 | 含义 |
|---|---|
| 0 | 正常完成（含存在容错跳过项的情况） |
| 1 | 致命错误：registry 损坏、用法错误、add 参数校验失败 |
| 2 | 未提供命令或未知命令 |

## 已知限制与设计决策

- **单向镜像，不做双向合并**：target 中 source 从未有过的内容会被当作孤儿删除（含整个目录）。需要 target 侧自主内容时请勿纳入同步。
- **黑名单为硬编码**：点前缀 + `node_modules`，不可配置。
- **不镜像符号链接**：两侧符号链接保持原样、不参与同步。
- **跨进程并发**：多个 `sync` 进程可安全并发（只读 registry）；并发 `add`/`remove` 不设文件锁，为低频人工操作，冲突后重新执行即可。
- **registry 损坏不静默重建**：直接报错退出，防止丢失映射。

## 开发与验证

```bash
go vet ./...
bash verify.sh   # 15 项端到端验证：硬链/递归/增删/断链重链/黑名单/容错/计时
```

验证套件覆盖：add 镜像同 inode、嵌套目录物理建立、源删传播、僵尸目录清理、原子保存断链重链、`.git`/`node_modules`/点文件不镜像、target 缺失容错、3×400 文件毫秒级计时等。

## 规划文档

架构规划与决策记录见 [`.plan/hsync.md`](.plan/hsync.md)。

## License

[GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0)
