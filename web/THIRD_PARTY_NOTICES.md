# Third-Party Notices (web/)

本文件记录 `web/` 前端中使用的第三方素材及其许可来源。前端代码本身的上游模板许可见同目录 [`LICENSE`](LICENSE)（MIT，Copyright (c) 2024 Mohammed Arham Khan），项目整体许可见仓库根目录 `LICENSE`（AGPL-3.0），两者均未改动。

## Lucide「leaf」图标（Yezi 叶子标识）

| 项目 | 内容 |
| --- | --- |
| 来源项目 | Lucide — <https://lucide.dev> / <https://github.com/lucide-icons/lucide> |
| 原始文件 | `icons/leaf.svg` — <https://raw.githubusercontent.com/lucide-icons/lucide/main/icons/leaf.svg> |
| 图标贡献者 | `icons/leaf.json` 记录：karsa-mistmere、ericfennis |
| 许可 | ISC License，Copyright (c) 2026 Lucide Icons and Contributors（取自上游 `LICENSE`，取用日期 2026-10-13） |
| 备注 | `leaf` 不在 Lucide `LICENSE` 列出的 Feather 派生图标清单内，因此仅适用 ISC，无需附带 Feather 的 MIT 条款 |

### 使用位置与改动

两条路径数据（`<path d="M11 20a10 10 0 0010-10 …"/>` 与 `<path d="M2 21a5 5 0 012.911-4.544 …"/>`）保持上游原样，改动仅限呈现方式：

- `src/components/yezi-logo.tsx` — 内联 SVG，描边为 `currentColor`；`YeziLogoBadge` 在品牌绿（`emerald-600`）圆角底上叠加白色叶子。用于侧栏与登录/首次设置页。
- `src/app/icon.svg`、`src/app/icon.png`（512×512）、`src/app/favicon.ico`（16/32/48）— 网站图标：品牌绿（`#059669`）圆角底 + 白色叶子，描边加粗到 2.25 以保证小尺寸清晰；`icon.png` 与 `favicon.ico` 由 `icon.svg` 渲染而来。

页面以 YETRX 品牌显示，图标可访问名称（alt / aria-label）为 `Yezi`。

### ISC License（上游原文）

```
ISC License

Copyright (c) 2026 Lucide Icons and Contributors

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

> 许可核实状态：以上文字取自 Lucide 官方仓库 `main` 分支 `LICENSE`（2026-10-13 抓取）。图标研究 agent 若有不同结论，以其核实结果为准并更新本文件。

## `lucide-react` 依赖

`package.json` 已依赖 `lucide-react`（同为 Lucide，ISC），其它界面图标由它提供，许可随 npm 包分发，不在此重复。
