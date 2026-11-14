// Every element grelot shows comes from @cplieger/ui: the app's stylesheets
// hold only the language's import and the seed block, its code names only
// .cp-* classes, and it writes no inline style.
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const root = join(import.meta.dirname, "..");

const seedProperty = /^--cp-(accent|neutral|brand|danger|warning|success)-[hc]$/;
const languageImport = '@import url("@cplieger/ui/css/system")';
const classToken = /^cp(-[a-z0-9]+)*$/;

function stripCSSComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, "");
}

/** Top-level statements of a stylesheet: at-rules ending in `;` and blocks `prelude { body }`. */
function cssStatements(css: string): { prelude: string; body: string | null }[] {
  const out: { prelude: string; body: string | null }[] = [];
  let depth = 0;
  let start = 0;
  let bodyStart = -1;
  for (let i = 0; i < css.length; i++) {
    const c = css[i];
    if (c === "{") {
      if (depth === 0) {
        bodyStart = i;
      }
      depth++;
    } else if (c === "}") {
      depth--;
      if (depth === 0) {
        out.push({
          prelude: css.slice(start, bodyStart).trim(),
          body: css.slice(bodyStart + 1, i),
        });
        start = i + 1;
      }
    } else if (c === ";" && depth === 0) {
      out.push({ prelude: css.slice(start, i).trim(), body: null });
      start = i + 1;
    }
  }
  const rest = css.slice(start).trim();
  if (rest !== "" || depth !== 0) {
    out.push({ prelude: rest, body: null });
  }
  return out;
}

function cssViolations(css: string): string[] {
  const found: string[] = [];
  for (const { prelude, body } of cssStatements(stripCSSComments(css))) {
    if (body === null) {
      if (prelude !== languageImport) {
        found.push(`a statement other than the language's import: ${prelude}`);
      }
      continue;
    }
    if (prelude !== ":root") {
      found.push(`a rule other than the seed block: ${prelude}`);
      continue;
    }
    for (const decl of body.split(";").map((d) => d.trim())) {
      if (decl === "") {
        continue;
      }
      const [name = "", value = ""] = decl.split(":").map((p) => p.trim());
      if (!seedProperty.test(name) || !/^\d+(\.\d+)?$/.test(value)) {
        found.push(`a declaration other than a seed: ${decl}`);
      }
    }
  }
  return found;
}

/** A class list's tokens outside the language; an interpolation is one, since its value goes unchecked. */
function badTokens(list: string): string[] {
  return list
    .replace(/\$\{[^}]*\}/g, (s) => ` ${s.replace(/\s+/g, "")} `)
    .split(/\s+/)
    .filter((t) => t !== "" && !classToken.test(t));
}

const quoted = /(["'`])((?:(?!\1)[^\\]|\\.)*)\1/g;

function codeViolations(src: string): string[] {
  const found: string[] = [];
  const classLists = [
    /className\s*(?::|\+?=(?!=))\s*(["'`])((?:(?!\1)[^\\]|\\.)*)\1/g,
    /setAttribute\(\s*["']class["']\s*,\s*(["'`])((?:(?!\1)[^\\]|\\.)*)\1/g,
    /\bclass=(["'])((?:(?!\1)[^\\]|\\.)*)\1/g,
  ];
  for (const re of classLists) {
    for (const m of src.matchAll(re)) {
      for (const t of badTokens(m[2] ?? "")) {
        found.push(`a class outside the language: ${t}`);
      }
    }
  }
  // A class write whose value is not a literal could name any class, so it is refused too.
  for (const re of [
    /className\s*(?::|\+?=(?!=))\s*(?!["'`\s])[^\s,;)]+/g,
    /setAttribute\(\s*["']class["']\s*,\s*(?!["'`\s])[^\s,)]+/g,
  ]) {
    for (const m of src.matchAll(re)) {
      found.push(`a class the check cannot read: ${m[0]}`);
    }
  }
  for (const m of src.matchAll(/classList\.(add|remove|toggle|replace)\(([^)]*)\)/g)) {
    const all = m[2] ?? "";
    // toggle's second argument is its force flag, which may hold a string.
    const literal =
      m[1] === "toggle"
        ? /^\s*["'`]/.test(all)
        : all.replace(quoted, "").replace(/[\s,]/g, "") === "";
    if (!literal) {
      found.push(`a class the check cannot read: ${m[0]}`);
    }
    const args = m[1] === "toggle" ? [...all.matchAll(quoted)].slice(0, 1) : all.matchAll(quoted);
    for (const s of args) {
      for (const t of badTokens(s[2] ?? "")) {
        found.push(`a class outside the language: ${t}`);
      }
    }
  }
  for (const m of src.matchAll(/\.style\b(\.setProperty\(\s*["']([^"']*)["'])?/g)) {
    found.push(`an inline style write: ${m[0]}`);
  }
  for (const re of [
    /setAttribute\(\s*["']style["']/g,
    // The words Intl.*Format takes as its style option are no inline style:
    // the date, list and relative-time forms and NumberFormat's own.
    /\bstyle\s*:(?!\s*["'](?:long|short|narrow|decimal|percent|unit|currency)["'])/g,
    /\bcssText\b/g,
    /\bstyle=["']/g,
  ]) {
    for (const m of src.matchAll(re)) {
      found.push(`an inline style write: ${m[0]}`);
    }
  }
  return found;
}

function files(dir: string, keep: (name: string) => boolean): string[] {
  const out: string[] = [];
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) {
      out.push(...files(p, keep));
    } else if (keep(e.name)) {
      out.push(p);
    }
  }
  return out;
}

const shipped = (name: string): boolean =>
  name.endsWith(".ts") &&
  !name.endsWith(".test.ts") &&
  !name.endsWith(".gen.ts") &&
  !name.endsWith(".d.ts");

describe("the app's stylesheets", () => {
  it("hold only the language's import and the seed block", () => {
    const sheets = files(join(root, "src"), (n) => n.endsWith(".css"));
    expect(sheets.length).toBeGreaterThan(0);
    const found = sheets.flatMap((f) =>
      cssViolations(readFileSync(f, "utf8")).map((v) => `${relative(root, f)}: ${v}`),
    );
    expect(found).toEqual([]);
  });
});

describe("the app's code and page", () => {
  it("name only .cp-* classes and write no inline style", () => {
    const sources = [...files(join(root, "src"), shipped), join(root, "index.html")];
    const found = sources.flatMap((f) =>
      codeViolations(readFileSync(f, "utf8")).map((v) => `${relative(root, f)}: ${v}`),
    );
    expect(found).toEqual([]);
  });
});

describe("cssViolations", () => {
  it("accepts the import and a seed block", () => {
    expect(
      cssViolations(
        `${languageImport};\n/* seeds */\n:root { --cp-accent-h: 200; --cp-neutral-c: 0.012; }`,
      ),
    ).toEqual([]);
  });

  it.each([
    [".frame { display: grid; }", "a rule other than the seed block: .frame"],
    [":root { --cp-accent-h: 200; color: red; }", "a declaration other than a seed: color: red"],
    [
      ":root { --cp-surface-page: white; }",
      "a declaration other than a seed: --cp-surface-page: white",
    ],
    [
      ":root { --cp-accent-h: var(--x); }",
      "a declaration other than a seed: --cp-accent-h: var(--x)",
    ],
    [
      '@import "./mine.css";',
      'a statement other than the language\'s import: @import "./mine.css"',
    ],
    [
      "@media (width < 40rem) { :root { --cp-accent-h: 1; } }",
      "a rule other than the seed block: @media (width < 40rem)",
    ],
    ["@layer app;", "a statement other than the language's import: @layer app"],
  ])("refuses %s", (css, want) => {
    expect(cssViolations(css)).toEqual([want]);
  });
});

describe("codeViolations", () => {
  it("accepts the language's classes", () => {
    const src = [
      'el("div", { className: "cp-app" });',
      'if (node.className === "cp-chip") {}',
      'node.classList.add("cp-feed", "cp-list-row");',
      'node.classList.toggle("cp-mono", kind === "path");',
      'enhanceRelativeTime(time, { style: "narrow", signal });',
      'new Intl.NumberFormat(locale, { style: "unit", unit: "minute" });',
      'new Intl.NumberFormat(locale, { style: "percent" });',
      'node.setAttribute("class", "cp");',
      '<html class="cp">',
    ].join("\n");
    expect(codeViolations(src)).toEqual([]);
  });

  it.each([
    ['el("div", { className: "frame" });', "a class outside the language: frame"],
    ["node.className = `cp-chip ${tone} wide`;", "a class outside the language: wide"],
    ["node.className = `cp-chip ${tone}`;", "a class outside the language: ${tone}"],
    ['node.className += " wide";', "a class outside the language: wide"],
    ['const c = "wide";\nnode.className = c;', "a class the check cannot read: className = c"],
    [
      'el("div", { className: wide ? "cp-a" : "b" });',
      "a class the check cannot read: className: wide",
    ],
    ["node.classList.add(name);", "a class the check cannot read: classList.add(name)"],
    [
      'node.classList.add("cp-a", name);',
      'a class the check cannot read: classList.add("cp-a", name)',
    ],
    [
      "node.classList.toggle(name, on);",
      "a class the check cannot read: classList.toggle(name, on)",
    ],
    [
      'node.setAttribute("class", name);',
      'a class the check cannot read: setAttribute("class", name',
    ],
    ['node.classList.toggle("open", on);', "a class outside the language: open"],
    ['node.classList.replace("cp-chip", "chip");', "a class outside the language: chip"],
    ['node.setAttribute("class", "cp-app shell");', "a class outside the language: shell"],
    ['<div class="layout">', "a class outside the language: layout"],
    ['node.style.color = "red";', "an inline style write: .style"],
    [
      'bar.style.setProperty("--cp-progress", "40%");',
      'an inline style write: .style.setProperty("--cp-progress"',
    ],
    [
      'node.style.setProperty("--gap", "4px");',
      'an inline style write: .style.setProperty("--gap"',
    ],
    ['el("div", { style: "color: red" });', "an inline style write: style:"],
    ['el("div", { style: "narrow-ish" });', "an inline style write: style:"],
    ['el("div", { style: "unit-width: 2px" });', "an inline style write: style:"],
    ['node.setAttribute("style", "x");', 'an inline style write: setAttribute("style"'],
    ['node.style.cssText = "";', "an inline style write: .style"],
    ['<p style="margin: 0">', 'an inline style write: style="'],
  ])("refuses %s", (src, want) => {
    expect(codeViolations(src)).toContain(want);
  });
});
