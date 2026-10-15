# SQL 工作台 XLSX 导出

## 目标与范围

下载的官网 SQL Editor 文档支持 CSV、JSON、XLSX。此增量在已有 ResultExports 中增加 XLSX 下载，覆盖单条查询和每条脚本结果；无结果列不显示按钮。不增加后端接口、数据库表或导出服务，不再次提交 SQL，也不唤醒 Compute。

基于 [Microsoft SpreadsheetML 结构](https://learn.microsoft.com/en-us/office/open-xml/spreadsheet/structure-of-a-spreadsheetml-document)生成 OpenXML 工作簿，使用锁定的 fflate 0.8.3 ZIP 实现。npm lock 的 SHA512 与下载包独立核对，上游 MIT 许可保留在 third_party/licenses 和前端 /licenses/fflate-0.8.3-MIT.txt。

## 数据与安全合同

- Result 工作表保留列的位置与重复/空列名；第一行冻结，随后是当前已显示的数据。
- 所有非 NULL 值均显式写为文本，包括数字、布尔值、JSON、时间及以 =/+/-/@ 开头的内容。不会创建公式、宏、超链接或外部关系，不把 SQL 文本交给 Excel 计算。
- int8/numeric 等在 API 已以字符串返回的精度得到保留；不能恢复 API/JavaScript 已经舍入的数据。需要 JSON 类型时使用 JSON 下载，不能把文本型 XLSX 冒充有 PostgreSQL 类型系统的备份。
- NULL 是空单元格，空字符串是存在的 inline string；电子表格显示可能相同，XML 表达不同。
- Unicode、换行与 XML 特殊字符转义；XML 禁用控制字符使用 SpreadsheetML 转义，原始 _xNNNN_ 文本防止被误解释。
- Export info 工作表包含格式版本、命令标签、实际导出行/列数、truncated 和范围说明。截断结果只导出收到的子集，不读取未显示的数据。
- 最多 100000 个单元格、每格 32767 个 UTF-16 code units、累计 8 MiB 文本。超限明确报错并建议 JSON 或缩小查询；不静默截断单元格。该限制用于有界浏览器导出，不是全库批量导出。
- 结果仅在浏览器内打包。下载 Blob URL 延迟释放，不持久保存到服务端，不包含 SQL 密码、会话或连接串。

## 验收与复测

仅新增 tests/result-xlsx.test.ts：解 ZIP 检查所有关系和类型、公式文本、大整数字符串、重复列、NULL、Unicode/控制字符/转义字面量、AA 列名及超限拒绝。旧 CSV/JSON 测试无需重复。

Linux web/e2e/sql-xlsx.spec.ts 从真实 UI 登录与 SQL 开始，下载 query-result.xlsx，再独立解析 ZIP 中所有 XML，校验实际结果、精度与公式安全，并确认下载没有新增 SQL。结束正常缩零，保留工作簿、截图、原身份和数据。具体镜像、版本、实测结果以对应交付回执为准；浏览器下载成功不代表所有 Excel/LibreOffice 版本都已经人工兼容认证。
