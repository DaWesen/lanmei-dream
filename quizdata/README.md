# 编程答题题库说明

蓝妹的编程答题（`/答题`）题库已外部化到本目录。添加题目或新增语言**无需修改任何 Go 代码**，只需按下面的约定放置 JSON 文件，系统会自动扫描加载并热更新。

## 目录结构

```
quizdata/
  java/questions.json     # 目录名即语言标识
  go/questions.json
  python/questions.json
  c/questions.json
  cpp/questions.json
  rust/questions.json     # 新增语言：建目录 + 放 JSON 即可
```

- **目录名 = 语言标识**（`quizLanguage`）。扫描器自动发现所有语言目录，未知语言同样会被加载。
- 每个语言目录下可放**一个或多个** `*.json` 文件，加载时会自动合并（例如可按难度拆成 `easy.json` / `medium.json` / `hard.json`）。
- 文件内容是一个**题目数组** `[ ... ]`，每个元素是一道题。

## 题目 JSON 字段规范

```json
{
  "id": "go-001",
  "prompt": "导入 fmt 包后，哪一项可以输出一行文字？",
  "options": [
    "fmt.PrintLine(\"Hi\")",
    "System.out.println(\"Hi\")",
    "fmt.Println(\"Hi\")",
    "console.log(\"Hi\")"
  ],
  "answer_index": 2,
  "explanation": "fmt.Println 会输出参数并在末尾换行。",
  "difficulty": "easy"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `id` | string | 是 | 全局唯一（跨语言也不可重复），建议用 `语言-序号` 命名，如 `go-001` |
| `prompt` | string | 是 | 题干 |
| `options` | string[] | 是 | **恰好 4 项**，非空且互不重复，依次对应 A / B / C / D |
| `answer_index` | int | 是 | 正确答案下标，`0`～`3` 依次对应 A～D |
| `explanation` | string | 是 | 解析说明 |
| `difficulty` | string | 否 | 难度，取值 `easy` / `medium` / `hard`；缺省自动视为 `easy` |

> 说明：`language` 字段**无需填写**，加载时始终由所在目录名决定，JSON 中的值会被忽略。
> 若在 JSON 里写了 `language`，仅作人类阅读参考，不会影响加载结果。

## 难度等级标准

| JSON 值 | 中文名 | 建议标准 |
| --- | --- | --- |
| `easy` | 简单 | 基本语法、单点知识 |
| `medium` | 中等 | 常见用法、需一定推理 |
| `hard` | 困难 | 进阶特性、易混淆边界 |

## 添加题目（推荐）

1. 进入对应语言目录（不存在则新建，目录名即语言标识）。
2. 新建或编辑 `*.json` 文件，按要求写入题目数组（参考上方示例）。
3. 保存即可，无需重启 —— 系统会在约 300ms 内检测到变更并热更新题库。

## 新增一门语言

1. 在 `quizdata/` 下新建目录，目录名即语言标识，例如 `rust`。
2. 在目录内放入符合规范的 `*.json`。
3. 保存即可。系统加载后，玩家可直接用 `/答题 rust` 出题。

可选优化（不改也能用）：

- 默认展示名 = 目录名首字母大写（`rust` → `Rust`），别名 = 目录名本身。
- 如需更友好的**展示名 / 别名 / 排序**，在
  [`internal/bizplugin/answer_question.go`](../internal/bizplugin/answer_question.go) 的
  `quizLanguageMetadata` 表中追加一条即可（例如为 `rust` 增加别名 `rs`、调整展示顺序），其余逻辑无需改动。

内置语言的别名与展示名：

| 目录名 | 展示名 | 别名 |
| --- | --- | --- |
| `java` | Java | `java` |
| `go` | Go | `go`, `golang` |
| `python` | Python | `python`, `py` |
| `c` | C | `c` |
| `cpp` | C++ | `c++`, `c＋＋`, `cpp` |

## 校验规则

加载时会对每道题做一致性校验，**任一题不符合都会导致整个题库加载失败**（并回退到上一次可用的快照），避免"半截题库"被启用：

- `id` / `prompt` / `explanation` 非空；
- `options` 恰好 4 项，且各项非空、互不重复；
- `answer_index` 在 `0`～`3` 之间；
- `difficulty` 为 `easy` / `medium` / `hard` 之一（或留空）；
- `id` 全局唯一。

## 使用（筛选题库）

每轮固定 5 题，每题限时 60 秒，每人每题最多作答 2 次。

| 命令 | 效果 |
| --- | --- |
| `/答题` | 全部语言混合出题，不限难度 |
| `/答题 go` | 仅 Go 题目 |
| `/答题 go python` | Go + Python 混合出题 |
| `/答题 中等` | 全部语言，只出中等题 |
| `/答题 rust 困难` | Rust 的困难题 |

语言支持别名（`golang`→Go、`py`→Python、`cpp`→C++），难度支持中英文（`简单`/`easy`、`中等`/`medium`、`困难`/`hard`）。