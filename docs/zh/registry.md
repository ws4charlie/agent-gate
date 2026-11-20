# AI 登记册

[English](../registry.md)

登记册是一份清单，列出所有被允许存在的模型、工具和智能体。每一项都有负责人、用途和状态。审计人员最先要的就是这份清单：你们运行了哪些 AI、谁负责、为什么要用。

## 一项记录包含什么

一项记录精确指向一个对象：模型用它的名称，工具写作 `server/tool`，智能体用它的 id。名称不是匹配模式，所以“有没有登记”只有是或否两种答案。负责人是一个人或一个团队，用途是一句平实的话，备注是自由文本。

另有两个字段：**风险等级**是 `low`、`medium`（默认）或 `high`；**版本**是自由文本，例如模型快照或工具发布号。

## 状态

- **proposed（待审）**：有人提出申请，还没人批准。这是默认值。
- **approved（已批准）**：可以使用。只有这个状态能通过下面的检查。
- **deprecated（已弃用）**：应当被替换，负责人需要迁移到别的对象。
- **blocked（已禁止）**：不得使用。

## 检查

网关用 `--registry-enforce`（或 `AG_REGISTRY_ENFORCE=true`）启动后，登记册就开始管事：每一次模型调用、工具调用和付费工具调用，只要对应的模型或工具没有状态为 `approved` 的记录，就会以规则 `registry_unapproved` 被拒绝。没有记录也算未批准。工具列表会隐藏未批准的工具，所以 Agent 看不到自己用不了的东西。

这项检查紧跟在 Agent 自己的允许清单之后。不在允许清单里的模型，仍然按 `scope_model` 或 `scope_tool` 拒绝；在清单里但未批准的，才是 `registry_unapproved`。Cedar 和内置规则执行同一条检查，结论一致。登记册读不出来时，请求会被拒绝。

这个开关默认关闭。关闭时不做任何查询，行为与以前完全一样。

## 如何获得批准

在命令行批准记录，每次变更都有记录。

```bash
agentgate registry add model gpt-4o --owner ml-team --purpose "客服对话" --risk medium --version 2026-05
agentgate registry status model gpt-4o approved
agentgate registry history model gpt-4o
```

`registry history` 列出每一次创建、状态变更和删除，以及操作人和时间；不带名称则列出全部。操作人默认是你的系统用户，也可用 `--actor` 指定。

`agentgate registry seed` 会把价格表里的每个模型，以及每个在用 Agent 允许清单里的确切模型名和工具名，凡是还没有记录的，都以 `approved` 加入，用途写作 “seeded from live config”。它不会改动已有记录，运行两次也没问题。`claude-*` 这样的通配符不会被写入，只会列出来。`--dry-run` 只显示会添加什么。开启开关之前先运行它，这样在用的东西不会被切断。

## 它能显示什么

登记册还会把清单和网关实际看到的使用情况做对比，有两个视图。一是在用但完全没登记的对象，附带调用次数和最近一次出现的时间。二是已登记为弃用或禁止、却仍在被使用的对象。两者默认都看最近 30 天。

控制台的 Registry 页签以只读方式展示清单和这两个视图。`agentgate registry gaps` 打印同样的视图。
