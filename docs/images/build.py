#!/usr/bin/env python3
"""Draw the docs diagrams. Run: python3 build.py (needs Google Chrome on macOS)."""
import pathlib
import subprocess
from html import escape

HERE = pathlib.Path(__file__).parent
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
FONT = "'PingFang SC','Hiragino Sans GB','Heiti SC',sans-serif"
T, S = 24, 19  # title and subtitle sizes; the canvas is about 1100 wide

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


class D:
    def __init__(self, name, w, h):
        self.name, self.w, self.h, self.o = name, w, h, []

    def box(self, x, y, w, h, title="", sub="", c="blue", size=T, dash=False):
        f, s = COL[c]
        d = ' stroke-dasharray="8 6"' if dash else ""
        self.o.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="{f}" stroke="{s}" stroke-width="2"{d}/>')
        lines = sub.split("\n") if sub else []
        total = (size if title else -8) + len(lines) * (S + 8)
        ty = y + (h - total) / 2 + (size * 0.85 if title else -S * 0.15 - 8)
        if title:
            self.text(x + w / 2, ty, title, size, INK, True)
        for i, l in enumerate(lines):
            self.text(x + w / 2, ty + (i + 1) * (S + 8), l, S, MUTED)

    def group(self, x, y, w, h, label, c="gray"):
        f, s = COL[c]
        self.o.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="16" fill="{f}" fill-opacity="0.45" stroke="{s}" stroke-width="2"/>')
        self.text(x + 22, y + 34, label, T, INK, True, "start")

    def text(self, x, y, t, size=S, fill=INK, bold=False, anchor="middle"):
        w = ' font-weight="600"' if bold else ""
        self.o.append(f'<text x="{x}" y="{y}" font-size="{size}" fill="{fill}" text-anchor="{anchor}"{w}>{escape(t)}</text>')

    def arrow(self, x1, y1, x2, y2, label="", c=MUTED, dash=False, both=False, lx=None, ly=None, anchor="middle"):
        d = ' stroke-dasharray="8 6"' if dash else ""
        st = ' marker-start="url(#hs)"' if both else ""
        self.o.append(f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{c}" stroke-width="2.5" marker-end="url(#h)"{st}{d}/>')
        if label:
            self.text(lx if lx is not None else (x1 + x2) / 2, ly if ly is not None else (y1 + y2) / 2 - 10, label, S, MUTED, False, anchor)

    def write(self):
        defs = (f'<defs><marker id="h" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto">'
                f'<path d="M0 0L10 5L0 10z" fill="{MUTED}"/></marker>'
                f'<marker id="hs" viewBox="0 0 10 10" refX="1" refY="5" markerWidth="8" markerHeight="8" orient="auto">'
                f'<path d="M10 0L0 5L10 10z" fill="{MUTED}"/></marker></defs>')
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{self.w}" height="{self.h}" viewBox="0 0 {self.w} {self.h}" '
               f'font-family="{FONT}"><rect width="100%" height="100%" fill="#FFFFFF"/>{defs}{"".join(self.o)}</svg>')
        html = HERE / f".{self.name}.html"
        html.write_text(f'<!doctype html><meta charset="utf-8"><style>html,body{{margin:0;background:#fff}}</style>{svg}', encoding="utf-8")
        subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=2",
                        f"--window-size={self.w},{self.h}", f"--screenshot={HERE / (self.name + '.png')}", html.as_uri()],
                       check=True, capture_output=True, timeout=60)
        html.unlink()



def row(d, items, y=30, h=120, x0=30, total=1080, gap=26, size=23):
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
    d = D("request-flow", 1140, 400)
    xs = row(d, [("Authenticate", "who is calling", "blue"), ("Policy", "is it allowed", "amber"), ("Hold", "lock worst case", "amber"),
                 ("Execute", "call upstream", "green"), ("Settle", "charge real cost", "amber"), ("Audit", "log the outcome", "purple")], gap=18)
    ys = cards(d, [("Refused", "told which rule,\nupstream never called", "red"), ("Needs a human", "funds stay held", "gray"),
                   ("Fails midway", "pay for what arrived,\nthe rest is returned", "gray"), ("Process dies", "a cleanup job\ncloses the hold later", "gray")], 240, 130)
    for a, b in zip(xs[1:5], ys):
        d.arrow(a, 150, b, 240)
    d.write()


def gateway():
    d = D("gateway", 1140, 410)
    d.box(30, 30, 280, 130, "Agent", "holds a gateway key", "white")
    d.box(430, 30, 280, 130, "Gateway", "holds the real key", "blue")
    d.box(830, 30, 280, 130, "Model provider", "OpenRouter and others", "gray")
    d.arrow(310, 75, 430, 75, "request", ly=64)
    d.arrow(430, 125, 310, 125, "stream", ly=114)
    d.arrow(710, 75, 830, 75, "request", ly=64)
    d.arrow(830, 125, 710, 125, "stream", ly=114)
    d.text(570, 215, "When something goes wrong", T, INK, True)
    cards(d, [("Stream dies", "pay for what arrived", "gray"), ("Provider error", "pay nothing", "gray"), ("Sent twice", "second one refused", "gray")], 245, 130)
    d.write()


def ledger():
    d = D("ledger", 1140, 410)
    xs = row(d, [("Balance", "$10.00", "purple"), ("Hold", "up to $0.50", "amber"), ("Call runs", "really costs $0.12", "green")], total=740, h=130)
    d.box(880, 30, 230, 130, "Charge $0.12", "return $0.38", "amber")
    d.arrow(770, 95, 880, 95)
    d.text(570, 215, "If the gateway crashes during the call", T, INK, True)
    row(d, [("Hold expires", "nothing closed it", "gray"), ("Background job", "looks for the real cost", "blue"), ("Close the hold", "charge it, or return it", "amber")], y=245, h=130)
    d.write()


def policy():
    d = D("policy", 1140, 400)
    xs = row(d, [("Active?", "not killed", "red"), ("In scope?", "model, tool, payee", "amber"), ("In budget?", "today's limit", "amber"),
                 ("Too fast?", "spend per minute", "amber"), ("Too large?", "ask a person", "blue")], gap=22, size=23)
    cards(d, [("Fails a check", "refused, and told which rule", "red"), ("Too large", "money held, waits for a person", "gray"), ("Passes all five", "goes ahead", "green")], 245, 130)
    d.text(570, 208, "Checks run left to right. The first failure decides.", T, INK, True)
    d.write()


def tools():
    d = D("tools", 1140, 410)
    d.box(30, 40, 260, 120, "Agent", "sees only allowed tools", "white")
    d.box(440, 40, 260, 120, "Gateway", "checks and charges", "blue")
    d.box(850, 40, 260, 120, "Tool", "keeps its own login", "gray")
    d.arrow(290, 100, 440, 100, "call", ly=88)
    d.arrow(700, 100, 850, 100, "call", ly=88)
    d.text(570, 212, "What happens to the result", T, INK, True)
    row(d, [("Tool result", "text someone else wrote", "gray"), ("Screen", "looks for known tricks", "amber")], y=245, h=130, total=660, gap=40)
    d.box(770, 240, 340, 62, "", "Clean: agent reads it", "green")
    d.box(770, 318, 340, 62, "", "Match: blocked and logged", "red")
    d.arrow(690, 290, 770, 272)
    d.arrow(690, 330, 770, 348)
    d.write()


def payments():
    d = D("payments", 1140, 420)
    row(d, [("Tool asks", "this costs 1 cent", "gray"), ("Policy", "may it pay?", "amber"), ("Wallet signs", "only if approved", "green"),
            ("Tool cashes it", "sends it to the chain", "gray"), ("Ledger charges", "the amount paid", "amber")], gap=22)
    d.text(570, 205, "If the tool reports anything but success", T, INK, True)
    d.box(30, 240, 400, 140, "Wait", "keep the money held until\nthe signature expires", "gray")
    d.box(530, 240, 250, 140, "Ask the chain", "did money move?", "blue")
    d.box(860, 240, 250, 62, "", "Moved: charge", "amber")
    d.box(860, 318, 250, 62, "", "Didn't move: refund", "green")
    d.arrow(430, 310, 530, 310)
    d.arrow(780, 295, 860, 272)
    d.arrow(780, 330, 860, 348)
    d.write()


def runs():
    d = D("runs", 1140, 360)
    d.group(30, 20, 1080, 230, "Run: write the weekly report", "blue")
    row(d, [("Model", "$0.02", "white"), ("Tool", "free", "white"), ("Payment", "$0.01", "white"), ("Model", "$0.03", "white")], x0=60, y=80, h=130, total=1020, gap=40)
    cards(d, [("Read it", "what happened, step by step", "gray"), ("Total it", "$0.06, taken from the ledger", "purple"), ("Stop it", "kill just this job", "red")], 266, 80, gap=20)
    d.write()


def sdk():
    d = D("sdk", 1140, 400)
    row(d, [("Your agent", "Python code", "white"), ("SDK", "small client", "blue"), ("Gateway", "says no, with a reason", "amber")], gap=60, h=110)
    d.text(570, 188, "The refusal becomes a clear error", T, INK, True)
    cards(d, [("BudgetExceeded", "stop for today", "red"), ("ScopeDenied", "try something else", "red"), ("Killed", "stop now", "red")], 215, 76)
    cards(d, [("ApprovalRequired", "wait for a person", "gray"), ("SettlementPending", "don't pay again", "gray")], 306, 76)
    d.write()


def testing():
    d = D("testing", 1140, 400)
    rows = [("Fakes", "Fake provider, simulated chain.\nCovers crashes, races and dropped streams.", "Every change", "blue"),
            ("Local end to end", "Real local chain, stand-in USDC, scripted model.\nThe whole path, with no keys.", "Every change", "green"),
            ("Live", "Real provider and network.\nOne small run so far.", "By hand", "amber")]
    y = 30
    for t, s, w, c in rows:
        d.box(30, y, 260, 104, t, "", c)
        d.box(306, y, 620, 104, "", s, "white")
        d.box(942, y, 168, 104, w, "", c, size=21)
        y += 118
    d.write()


for f in (request_flow, gateway, ledger, policy, tools, payments, runs, sdk, testing):
    f()
print("ok")
