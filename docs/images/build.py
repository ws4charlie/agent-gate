#!/usr/bin/env python3
"""Draw the docs diagrams. Run: python3 build.py (needs Google Chrome on macOS)."""
import pathlib
import subprocess
from html import escape

HERE = pathlib.Path(__file__).parent
OUT, LANG = HERE, "en"
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
FONT = "'PingFang SC','Hiragino Sans GB','Heiti SC',sans-serif"
T, S = 24, 20  # title and subtitle sizes; the canvas is about 1100 wide

COL = {
    "blue": ("#E8F0FE", "#3B6FD4"),
    "green": ("#E6F4EA", "#2E8B57"),
    "amber": ("#FEF3DC", "#C98A0C"),
    "red": ("#FCE8E6", "#C8453B"),
    "purple": ("#F1EAFB", "#7A52C7"),
    "gray": ("#F1F3F5", "#6B7480"),
    "white": ("#FFFFFF", "#AEB6BF"),
}
INK, MUTED = "#1F2933", "#52606D"


ZH = {
    'Authenticate': '认证',
    'who is calling': '谁在调用',
    'Policy': '策略',
    'is it allowed': '允不允许',
    'Hold': '冻结',
    'hold worst case': '冻结最坏花费',
    'Execute': '执行',
    'call upstream': '调用上游',
    'Settle': '结算',
    'real cost': '按实收费',
    'Audit': '记录',
    'log it': '写入审计',
    'Refused': '拒绝',
    'told which rule,': '告知命中的规则，',
    'upstream never called': '不调用上游',
    'Needs a human': '等人审批',
    'funds stay held': '钱继续冻结',
    'Fails midway': '中途失败',
    'pay for what arrived,': '只付已收到的部分，',
    'the rest is returned': '其余退回',
    'Process dies': '进程崩溃',
    'a cleanup job': '后台任务',
    'closes the hold later': '稍后关闭冻结',
    'Agent': 'Agent',
    'holds a gateway key': '只拿网关密钥',
    'Gateway': '网关',
    'holds the real key': '保管真实密钥',
    'Model provider': '模型供应商',
    'OpenRouter and others': 'OpenRouter 等',
    'request': '请求',
    'stream': '流式返回',
    'When something goes wrong': '出问题时怎么办',
    'Stream dies': '流中断',
    'pay for what arrived': '只付已收到的部分',
    'Provider error': '供应商报错',
    'pay nothing': '不收钱',
    'Sent twice': '重复发送',
    'second one refused': '第二次被拒绝',
    'Balance': '余额',
    'up to $0.50': '最多 $0.50',
    'Call runs': '调用',
    'really costs $0.12': '实际花了 $0.12',
    'Charge $0.12': '收 $0.12',
    'return $0.38': '退回 $0.38',
    'If the gateway crashes during the call': '如果调用中途网关崩溃',
    'Hold expires': '冻结到期',
    'nothing closed it': '没人关闭它',
    'Background job': '后台任务',
    'looks for the real cost': '查找真实花费',
    'Close the hold': '关闭冻结',
    'charge it, or return it': '按结果扣费或退回',
    'Active?': '还在用吗',
    'not killed': '没被停用',
    'In scope?': '在范围内吗',
    'model, tool, payee': '模型、工具、收款方',
    'In budget?': '预算够吗',
    "today's limit": '今日额度',
    'Too fast?': '花得太快吗',
    'spend per minute': '每分钟花费',
    'Too large?': '金额太大吗',
    'ask a person': '要人批准',
    'Fails a check': '没通过',
    'refused, and told which rule': '拒绝，并告知是哪条规则',
    'Too large': '金额太大',
    'money held, waits for a person': '钱冻结着，等人批准',
    'Passes all five': '全部通过',
    'goes ahead': '放行',
    'Checks run left to right. The first failure decides.': '从左到右检查，第一个不通过的说了算。',
    'sees only allowed tools': '只看到允许的工具',
    'checks and charges': '检查并收费',
    'Tool': '工具',
    'keeps its own login': '凭证留在网关',
    'call': '调用',
    'What happens to the result': '工具返回的结果怎么处理',
    'Tool result': '工具结果',
    'text someone else wrote': '别人写的文字',
    'Screen': '筛查',
    'looks for known tricks': '查找已知手法',
    'Clean: agent reads it': '干净：交给 Agent',
    'Match: blocked and logged': '命中：拦截并记录',
    'Tool asks': '工具要钱',
    'this costs 3 cents': '这次收 3 美分',
    'may it pay?': '可以付吗',
    'Wallet signs': '钱包签名',
    'only if approved': '批准后才签',
    'Tool cashes it': '工具兑现',
    'sends it to chain': '提交到链上',
    'Ledger charges': '账本扣费',
    'the amount paid': '扣实付金额',
    'If the tool reports anything but success': '只要工具没报成功',
    'Wait': '等待',
    'keep the money held until': '钱继续冻结，',
    'the signature expires': '直到签名过期',
    'Ask the chain': '问区块链',
    'did money move?': '钱动了吗',
    'Moved: charge': '动了：扣费',
    "Didn't move: release hold": '没动：解除冻结',
    'Run: write the weekly report': '一次 Run：写周报',
    'Model': '模型',
    'free': '免费',
    'Payment': '支付',
    'Read it': '看过程',
    'what happened, step by step': '每一步做了什么',
    'Total it': '算总账',
    '$0.06, taken from the ledger': '$0.06，数据来自账本',
    'Stop it': '停下来',
    'kill just this job': '只停这项工作',
    'Your agent': '你的 Agent',
    'Python code': 'Python 代码',
    'small client': '轻量客户端',
    'says no, with a reason': '拒绝时给出原因',
    'The refusal becomes a clear error': '拒绝会变成清楚的错误',
    'stop for today': '今天先停',
    'try something else': '换个做法',
    'stop now': '立刻停止',
    'wait for a person': '等人批准',
    "don't pay again": '不要重复付款',
    'Fakes': '模拟环境',
    'Fake provider, simulated chain.': '模拟供应商和模拟链。',
    'Covers crashes, races and dropped streams.': '覆盖崩溃、并发和断流。',
    'Every change': '每次改动',
    'Local end to end': '本地端到端',
    'Real local chain, stand-in USDC, scripted model.': '真实本地链、替身 USDC、脚本化模型。',
    'The whole path, with no keys.': '跑通全流程，不需要密钥。',
    'Live': '线上',
    'Real provider and network.': '真实供应商和网络。',
    'Early, with more to come.': '刚开始，后续继续测。',
    'your code, with its own key': '你的代码，用自己的密钥',
    'Operator': '运营方',
    'sets limits by CLI': '用命令行设限额',
    'one process that keeps the provider keys': '一个进程，保管供应商密钥',
    'Postgres': 'Postgres',
    'holds all the state': '存放全部状态',
    'Every request passes all three': '每个请求都要过这三关',
    'Then it takes one of three paths': '然后走三条路之一',
    'may it do this?': '能不能做',
    'Ledger': '账本',
    'hold, then charge': '先冻结，后扣费',
    'Runs': 'Run',
    'record every step': '记录每一步',
    'Models': '模型',
    'proxy the call': '转发调用',
    'Tools': '工具',
    'check, then screen': '检查后筛查结果',
    'Payments': '支付',
    'wallet signs if approved': '批准后钱包才签名',
    'Model providers': '模型供应商',
    'OpenRouter, OpenAI…': 'OpenRouter、OpenAI…',
    'MCP tools': 'MCP 工具',
    'services run by others': '别人运行的服务',
    'Blockchain': '区块链',
    'the tool cashes, we verify': '工具兑现，网关核对',
    'By hand': '人工',
}


class D:
    def __init__(self, name, w, h):
        self.name, self.w, self.h, self.o = name, w, h, []

    def box(self, x, y, w, h, title="", sub="", c="blue", size=T, dash=False, sub_size=None):
        f, s = COL[c]
        d = ' stroke-dasharray="8 6"' if dash else ""
        self.o.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="{f}" stroke="{s}" stroke-width="2"{d}/>')
        lines = sub.split("\n") if sub else []
        sz = sub_size or S
        total = (size if title else -8) + len(lines) * (sz + 8)
        ty = y + (h - total) / 2 + (size * 0.85 if title else -sz * 0.15 - 8)
        if title:
            self.text(x + w / 2, ty, title, size, INK, True)
        for i, l in enumerate(lines):
            self.text(x + w / 2, ty + (i + 1) * (sz + 8), l, sz, MUTED)

    def group(self, x, y, w, h, label, c="gray"):
        f, s = COL[c]
        self.o.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="16" fill="{f}" fill-opacity="0.45" stroke="{s}" stroke-width="2"/>')
        self.text(x + 22, y + 34, label, T, INK, True, "start")

    def text(self, x, y, t, size=S, fill=INK, bold=False, anchor="middle"):
        if LANG == "zh":
            t = ZH.get(t, t)
        w = ' font-weight="600"' if bold else ""
        self.o.append(f'<text x="{x}" y="{y}" font-size="{size}" fill="{fill}" text-anchor="{anchor}"{w}>{escape(t)}</text>')

    def arrow(self, x1, y1, x2, y2, label="", c=MUTED, dash=False, both=False, lx=None, ly=None, anchor="middle"):
        d = ' stroke-dasharray="8 6"' if dash else ""
        st = ' marker-start="url(#hs)"' if both else ""
        self.o.append(f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{c}" stroke-width="2.5" marker-end="url(#h)"{st}{d}/>')
        if label:
            self.text(lx if lx is not None else (x1 + x2) / 2, ly if ly is not None else (y1 + y2) / 2 - 10, label, S, MUTED, False, anchor)

    def write(self):
        defs = (f'<defs><marker id="h" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">'
                f'<path d="M0 0L10 5L0 10z" fill="{MUTED}"/></marker>'
                f'<marker id="hs" viewBox="0 0 10 10" refX="1" refY="5" markerWidth="6" markerHeight="6" orient="auto">'
                f'<path d="M10 0L0 5L10 10z" fill="{MUTED}"/></marker></defs>')
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{self.w}" height="{self.h}" viewBox="0 0 {self.w} {self.h}" '
               f'font-family="{FONT}"><rect width="100%" height="100%" fill="#FFFFFF"/>{defs}{"".join(self.o)}</svg>')
        html = OUT / f".{self.name}.html"
        html.write_text(f'<!doctype html><meta charset="utf-8"><style>html,body{{margin:0;background:#fff}}</style>{svg}', encoding="utf-8")
        subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=2",
                        f"--window-size={self.w},{self.h}", f"--screenshot={OUT / (self.name + '.png')}", html.as_uri()],
                       check=True, capture_output=True, timeout=60)
        html.unlink()



def row(d, items, y=30, h=120, x0=30, total=1080, gap=44, size=23):
    n = len(items)
    w = (total - gap * (n - 1)) / n
    xs = []
    for i, (t, s, c) in enumerate(items):
        x = x0 + i * (w + gap)
        d.box(x, y, w, h, t, s, c, size=size)
        if i:
            d.arrow(x - gap, y + h / 2, x, y + h / 2)
        xs.append(x + w / 2)
    return xs


def cards(d, items, y, h=120, x0=30, total=1080, gap=20):
    n = len(items)
    w = (total - gap * (n - 1)) / n
    xs = []
    for i, (t, s, c) in enumerate(items):
        x = x0 + i * (w + gap)
        d.box(x, y, w, h, t, s, c, size=23)
        xs.append(x + w / 2)
    return xs


def request_flow():
    d = D("request-flow", 1140, 296)
    xs = row(d, [("Authenticate", "who is calling", "blue"), ("Policy", "is it allowed", "amber"), ("Hold", "hold worst case", "amber"),
                 ("Execute", "call upstream", "green"), ("Settle", "real cost", "amber"), ("Audit", "log it", "purple")], y=20, h=88, gap=36, size=21)
    ys = cards(d, [("Refused", "told which rule,\nupstream never called", "red"), ("Needs a human", "funds stay held", "gray"),
                   ("Fails midway", "pay for what arrived,\nthe rest is returned", "gray"), ("Process dies", "a cleanup job\ncloses the hold later", "gray")], 176, 100)
    for a, b in zip(xs[1:5], ys):
        d.arrow(a, 108, b, 176)
    d.write()


def gateway():
    d = D("gateway", 1140, 276)
    d.box(30, 20, 280, 92, "Agent", "holds a gateway key", "white")
    d.box(430, 20, 280, 92, "Gateway", "holds the real key", "blue")
    d.box(830, 20, 280, 92, "Model provider", "OpenRouter and others", "gray")
    d.arrow(310, 48, 430, 48, "request", ly=38)
    d.arrow(430, 88, 310, 88, "stream", ly=78)
    d.arrow(710, 48, 830, 48, "request", ly=38)
    d.arrow(830, 88, 710, 88, "stream", ly=78)
    d.text(570, 150, "When something goes wrong", T, INK, True)
    cards(d, [("Stream dies", "pay for what arrived", "gray"), ("Provider error", "pay nothing", "gray"), ("Sent twice", "second one refused", "gray")], 170, 88)
    d.write()


def ledger():
    d = D("ledger", 1140, 276)
    xs = row(d, [("Balance", "$10.00", "purple"), ("Hold", "up to $0.50", "amber"), ("Call runs", "really costs $0.12", "green")], total=740, y=20, h=92)
    d.box(880, 20, 230, 92, "Charge $0.12", "return $0.38", "amber")
    d.arrow(770, 66, 880, 66)
    d.text(570, 150, "If the gateway crashes during the call", T, INK, True)
    row(d, [("Hold expires", "nothing closed it", "gray"), ("Background job", "looks for the real cost", "blue"), ("Close the hold", "charge it, or return it", "amber")], y=170, h=88)
    d.write()


def policy():
    d = D("policy", 1140, 266)
    xs = row(d, [("Active?", "not killed", "red"), ("In scope?", "model, tool, payee", "amber"), ("In budget?", "today's limit", "amber"),
                 ("Too fast?", "spend per minute", "amber"), ("Too large?", "ask a person", "blue")], y=20, h=88, gap=36, size=23)
    cards(d, [("Fails a check", "refused, and told which rule", "red"), ("Too large", "money held, waits for a person", "gray"), ("Passes all five", "goes ahead", "green")], 160, 88)
    d.text(570, 140, "Checks run left to right. The first failure decides.", T, INK, True)
    d.write()


def tools():
    d = D("tools", 1140, 286)
    d.box(30, 20, 260, 92, "Agent", "sees only allowed tools", "white")
    d.box(440, 20, 260, 92, "Gateway", "checks and charges", "blue")
    d.box(850, 20, 260, 92, "Tool", "keeps its own login", "gray")
    d.arrow(290, 66, 440, 66, "call", ly=54)
    d.arrow(700, 66, 850, 66, "call", ly=54)
    d.text(570, 150, "What happens to the result", T, INK, True)
    row(d, [("Tool result", "text someone else wrote", "gray"), ("Screen", "looks for known tricks", "amber")], y=170, h=96, total=660, gap=40)
    d.box(770, 170, 340, 44, "", "Clean: agent reads it", "green")
    d.box(770, 222, 340, 44, "", "Match: blocked and logged", "red")
    d.arrow(690, 208, 770, 192)
    d.arrow(690, 228, 770, 244)
    d.write()


def payments():
    d = D("payments", 1140, 280)
    row(d, [("Tool asks", "this costs 3 cents", "gray"), ("Policy", "may it pay?", "amber"), ("Wallet signs", "only if approved", "green"),
            ("Tool cashes it", "sends it to chain", "gray"), ("Ledger charges", "the amount paid", "amber")], y=20, h=88, gap=36, size=21)
    d.text(570, 140, "If the tool reports anything but success", T, INK, True)
    d.box(30, 160, 400, 100, "Wait", "keep the money held until\nthe signature expires", "gray")
    d.box(530, 160, 250, 100, "Ask the chain", "did money move?", "blue")
    d.box(860, 160, 250, 46, "", "Moved: charge", "amber")
    d.box(860, 214, 250, 46, "", "Didn't move: release hold", "green")
    d.arrow(430, 210, 530, 210)
    d.arrow(780, 198, 860, 183)
    d.arrow(780, 222, 860, 237)
    d.write()


def runs():
    d = D("runs", 1140, 276)
    d.group(30, 20, 1080, 156, "Run: write the weekly report", "blue")
    row(d, [("Model", "$0.02", "white"), ("Tool", "free", "white"), ("Payment", "$0.01", "white"), ("Model", "$0.03", "white")], x0=60, y=68, h=92, total=1020, gap=40)
    cards(d, [("Read it", "what happened, step by step", "gray"), ("Total it", "$0.06, taken from the ledger", "purple"), ("Stop it", "kill just this job", "red")], 190, 72, gap=20)
    d.write()


def sdk():
    d = D("sdk", 1140, 322)
    row(d, [("Your agent", "Python code", "white"), ("SDK", "small client", "blue"), ("Gateway", "says no, with a reason", "amber")], y=20, gap=60, h=84)
    d.text(570, 138, "The refusal becomes a clear error", T, INK, True)
    cards(d, [("BudgetExceeded", "stop for today", "red"), ("ScopeDenied", "try something else", "red"), ("Killed", "stop now", "red")], 158, 70)
    cards(d, [("ApprovalRequired", "wait for a person", "gray"), ("SettlementPending", "don't pay again", "gray")], 238, 70)
    d.write()


def testing():
    d = D("testing", 1140, 304)
    rows = [("Fakes", "Fake provider, simulated chain.\nCovers crashes, races and dropped streams.", "Every change", "blue"),
            ("Local end to end", "Real local chain, stand-in USDC, scripted model.\nThe whole path, with no keys.", "Every change", "green"),
            ("Live", "Real provider and network.\nEarly, with more to come.", "By hand", "amber")]
    y = 20
    for t, s, w, c in rows:
        d.box(30, y, 260, 84, t, "", c)
        d.box(306, y, 620, 84, "", s, "white")
        d.box(942, y, 168, 84, w, "", c, size=21)
        y += 96
    d.write()


def architecture():
    # The README shows this as a grid of tiles so that each box can be a link.
    # Columns are 380 wide. Tiles carry their own margins, so they line up.
    def tile(name, w, h, title, sub, c, right=19):
        d = D(f"arch-{name}", w, h)
        d.box(19, 2, w - 19 - right, h - 4, title, sub, c, size=26, sub_size=21)
        return d

    def bar(d, y, x1, x2):
        d.o.append(f'<line x1="{x1}" y1="{y}" x2="{x2}" y2="{y}" stroke="{MUTED}" stroke-width="2.5"/>')

    def stub(d, x, y1, y2):
        d.o.append(f'<line x1="{x}" y1="{y1}" x2="{x}" y2="{y2}" stroke="{MUTED}" stroke-width="2.5"/>')

    H, cols = 78, (190, 570, 950)
    tile("agent", 760, H, "Agent", "your code, with its own key", "blue", right=99).write()
    tile("operator", 380, H, "Operator", "sets limits by CLI", "gray").write()

    d = D("arch-down", 1140, 30)
    d.arrow(340, 2, 340, 28)
    d.arrow(950, 2, 950, 28)
    d.write()

    d = tile("gateway", 760, H, "Gateway", "one process that keeps the provider keys", "blue", right=99)
    d.arrow(760, H / 2, 661, H / 2)
    d.write()
    d = tile("postgres", 380, H, "Postgres", "holds all the state", "gray")
    d.arrow(0, H / 2, 19, H / 2)
    d.write()

    d = D("arch-split", 1140, 58)
    stub(d, 340, 2, 28)
    bar(d, 28, cols[0], cols[2])
    for x in cols:
        d.arrow(x, 28, x, 56)
    d.text(645, 21, "Every request passes all three", 24, MUTED)
    d.write()

    tile("policy", 380, H, "Policy", "may it do this?", "amber").write()
    tile("ledger", 380, H, "Ledger", "hold, then charge", "amber").write()
    tile("runs", 380, H, "Runs", "record every step", "amber").write()

    d = D("arch-merge", 1140, 58)
    for x in cols:
        stub(d, x, 2, 28)
        d.arrow(x, 28, x, 56)
    bar(d, 28, cols[0], cols[2])
    d.text(380, 21, "Then it takes one of three paths", 24, MUTED)
    d.write()

    tile("models", 380, H, "Models", "proxy the call", "green").write()
    tile("tools", 380, H, "Tools", "check, then screen", "green").write()
    tile("payments", 380, H, "Payments", "wallet signs if approved", "green").write()

    d = D("arch-out", 1140, 30)
    for x in cols:
        d.arrow(x, 2, x, 28)
    d.write()

    tile("providers", 380, H, "Model providers", "OpenRouter, OpenAI…", "white").write()
    tile("mcp", 380, H, "MCP tools", "services run by others", "white").write()
    tile("chain", 380, H, "Blockchain", "the tool cashes, we verify", "white").write()


for OUT, LANG in ((HERE, "en"), (HERE.parent / "zh" / "images", "zh")):
    for f in (architecture, request_flow, gateway, ledger, policy, tools, payments, runs, sdk, testing):
        f()
print("ok")
