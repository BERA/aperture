/*
 * rules-serializer.test.js — round-trip correctness for the pure graph<->AST
 * serializer (E7-S2). This is the LOAD-BEARING test: it proves astToGraph and
 * graphToAST are lossless inverses over the exact E2-S3 AST shape, with no
 * browser and no dependencies.
 *
 * Run it directly with node (NOT part of the Go build or CI, which are node-free):
 *
 *   node internal/server/static/js/rules-serializer.test.js
 *
 * Exit code 0 = all round-trips + validation cases passed; 1 = a failure.
 */
"use strict";

const S = require("./rules-serializer.js");

let failures = 0;
function ok(cond, msg) {
  if (!cond) {
    failures++;
    console.error("FAIL: " + msg);
  } else {
    console.log("pass: " + msg);
  }
}
function eq(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

// --- Representative ASTs, byte-for-byte the rules.Node JSON shape ------------
// Covers: nested and/or/not, compare with var+literal, in/nin with a list,
// a call, the nine collection operators (E4-S1) including the three unary ones,
// and the scalar edge cases (false/0/""/null) that must survive.
//
// These mirror the cases in rules/editor_contract_test.go one-for-one — that Go
// test is the CI-guarded half of the same invariant, since CI is node-free.
const cases = {
  "compare var eq string": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.classification" },
    right: { type: "literal", value: "public" },
  },
  "nested and/or/not": {
    type: "and",
    children: [
      {
        type: "or",
        children: [
          { type: "compare", op: "gt", left: { type: "var", name: "object.level" }, right: { type: "literal", value: 3 } },
          { type: "compare", op: "eq", left: { type: "var", name: "principal.tier" }, right: { type: "literal", value: "gold" } },
        ],
      },
      {
        type: "not",
        children: [
          { type: "compare", op: "eq", left: { type: "var", name: "account.suspended" }, right: { type: "literal", value: true } },
        ],
      },
    ],
  },
  "in with list literal": {
    type: "compare",
    op: "in",
    left: { type: "var", name: "object.region" },
    right: {
      type: "list",
      items: [
        { type: "literal", value: "us" },
        { type: "literal", value: "eu" },
        { type: "literal", value: "apac" },
      ],
    },
  },
  "nin with var on right": {
    type: "compare",
    op: "nin",
    left: { type: "var", name: "principal.id" },
    right: { type: "var", name: "object.blocklist" },
  },
  "call len over var compared to number": {
    type: "compare",
    op: "ge",
    left: { type: "call", name: "len", items: [{ type: "var", name: "object.tags" }] },
    right: { type: "literal", value: 1 },
  },
  "nested call contains(lower(var), literal)": {
    type: "call",
    name: "contains",
    items: [
      { type: "call", name: "lower", items: [{ type: "var", name: "object.title" }] },
      { type: "literal", value: "secret" },
    ],
  },
  // The nine collection operators. The six binary ones keep the left/right
  // shape; the three unary ones carry NO `right` key at all.
  "has element": {
    type: "compare",
    op: "has",
    left: { type: "var", name: "object.tags" },
    right: { type: "literal", value: "urgent" },
  },
  "hasAll list": {
    type: "compare",
    op: "hasAll",
    left: { type: "var", name: "object.tags" },
    right: { type: "list", items: [{ type: "literal", value: "a" }, { type: "literal", value: "b" }] },
  },
  "hasAny list": {
    type: "compare",
    op: "hasAny",
    left: { type: "var", name: "object.tags" },
    right: { type: "list", items: [{ type: "literal", value: "a" }, { type: "literal", value: "b" }] },
  },
  "hasNone list": {
    type: "compare",
    op: "hasNone",
    left: { type: "var", name: "object.tags" },
    right: { type: "list", items: [{ type: "literal", value: "a" }] },
  },
  "subsetOf var": {
    type: "compare",
    op: "subsetOf",
    left: { type: "var", name: "object.tags" },
    right: { type: "var", name: "principal.allowedTags" },
  },
  hasKey: {
    type: "compare",
    op: "hasKey",
    left: { type: "var", name: "object.owner" },
    right: { type: "literal", value: "dept" },
  },
  "isEmpty unary": { type: "compare", op: "isEmpty", left: { type: "var", name: "object.tags" } },
  "isNotEmpty unary": { type: "compare", op: "isNotEmpty", left: { type: "var", name: "object.tags" } },
  "exists unary": { type: "compare", op: "exists", left: { type: "var", name: "object.owner.dept" } },
  "collection ops nested under and/or": {
    type: "and",
    children: [
      { type: "compare", op: "exists", left: { type: "var", name: "object.tags" } },
      {
        type: "or",
        children: [
          {
            type: "compare",
            op: "hasAny",
            left: { type: "var", name: "object.tags" },
            right: { type: "list", items: [{ type: "literal", value: "gold" }] },
          },
          {
            type: "not",
            children: [{ type: "compare", op: "isEmpty", left: { type: "var", name: "object.owner" } }],
          },
        ],
      },
    ],
  },
  // The eight date operators. Seven take a single element on the right; between
  // takes a list of EXACTLY TWO bounds and no new JSON key.
  "before literal date": {
    type: "compare",
    op: "before",
    left: { type: "var", name: "object.hired_at" },
    right: { type: "literal", value: "2026-01-01" },
  },
  "after literal datetime": {
    type: "compare",
    op: "after",
    left: { type: "var", name: "object.touched_at" },
    right: { type: "literal", value: "2026-01-01T00:00:00Z" },
  },
  "onOrBefore var": {
    type: "compare",
    op: "onOrBefore",
    left: { type: "var", name: "object.due_at" },
    right: { type: "var", name: "principal.deadline" },
  },
  "sameDay literal": {
    type: "compare",
    op: "sameDay",
    left: { type: "var", name: "object.hired_at" },
    right: { type: "literal", value: "2026-03-04" },
  },
  "sameMonth literal": {
    type: "compare",
    op: "sameMonth",
    left: { type: "var", name: "object.hired_at" },
    right: { type: "literal", value: "2026-03-04" },
  },
  "sameYear literal": {
    type: "compare",
    op: "sameYear",
    left: { type: "var", name: "object.hired_at" },
    right: { type: "literal", value: "2026-03-04" },
  },
  "between literal bounds": {
    type: "compare",
    op: "between",
    left: { type: "var", name: "object.hired_at" },
    right: {
      type: "list",
      items: [
        { type: "literal", value: "2026-01-01" },
        { type: "literal", value: "2026-12-31" },
      ],
    },
  },

  // The relativeDate operand. Every one of these mirrors a case in
  // rules/editor_contract_test.go one-for-one, and all four fields are always
  // present in the struct's key order: type, anchor, n, unit, snap.
  "relative date months ago": {
    type: "compare",
    op: "onOrAfter",
    left: { type: "var", name: "object.touched_at" },
    right: { type: "relativeDate", anchor: "NOW", n: -3, unit: "months", snap: "none" },
  },
  "relative date today start of year": {
    type: "compare",
    op: "onOrAfter",
    left: { type: "var", name: "object.touched_at" },
    right: { type: "relativeDate", anchor: "TODAY", n: 0, unit: "days", snap: "startOfYear" },
  },
  "relative date forward hours": {
    type: "compare",
    op: "before",
    left: { type: "var", name: "object.expires_at" },
    right: { type: "relativeDate", anchor: "NOW", n: 12, unit: "hours", snap: "none" },
  },
  "relative date end of quarter": {
    type: "compare",
    op: "onOrBefore",
    left: { type: "var", name: "object.due_at" },
    right: { type: "relativeDate", anchor: "TODAY", n: 1, unit: "quarters", snap: "endOfQuarter" },
  },
  "relative date same year": {
    type: "compare",
    op: "sameYear",
    left: { type: "var", name: "object.hired_at" },
    right: { type: "relativeDate", anchor: "NOW", n: -1, unit: "years", snap: "startOfWeek" },
  },
  "between relative bounds": {
    type: "compare",
    op: "between",
    left: { type: "var", name: "object.hired_at" },
    right: {
      type: "list",
      items: [
        { type: "relativeDate", anchor: "NOW", n: -5, unit: "years", snap: "startOfYear" },
        { type: "relativeDate", anchor: "TODAY", n: 0, unit: "days", snap: "endOfDay" },
      ],
    },
  },
  "between literal and relative": {
    type: "compare",
    op: "between",
    left: { type: "var", name: "object.hired_at" },
    right: {
      type: "list",
      items: [
        { type: "literal", value: "2026-01-01" },
        { type: "relativeDate", anchor: "NOW", n: -30, unit: "minutes", snap: "none" },
      ],
    },
  },
  "relative date nested under and": {
    type: "and",
    children: [
      {
        type: "compare",
        op: "eq",
        left: { type: "var", name: "object.classification" },
        right: { type: "literal", value: "public" },
      },
      {
        type: "compare",
        op: "after",
        left: { type: "var", name: "object.touched_at" },
        right: { type: "relativeDate", anchor: "NOW", n: -90, unit: "days", snap: "startOfDay" },
      },
    ],
  },
  "relative date on the LEFT of a date operator": {
    type: "compare",
    op: "before",
    left: { type: "relativeDate", anchor: "TODAY", n: 0, unit: "days", snap: "startOfDay" },
    right: { type: "var", name: "object.hired_at" },
  },

  "falsy scalars survive — false": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.archived" },
    right: { type: "literal", value: false },
  },
  "falsy scalars survive — zero": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.count" },
    right: { type: "literal", value: 0 },
  },
  "falsy scalars survive — empty string": {
    type: "compare",
    op: "ne",
    left: { type: "var", name: "object.note" },
    right: { type: "literal", value: "" },
  },
  "falsy scalars survive — null": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.owner" },
    right: { type: "literal", value: null },
  },
};

// --- Round-trip invariant: graphToAST(astToGraph(ast)) deep-equals ast ------
Object.keys(cases).forEach(function (name) {
  const ast = cases[name];
  const graph = S.astToGraph(ast);
  const back = S.graphToAST(graph);
  ok(eq(back, ast), "round-trip: " + name);
});

// --- astToGraph produces a single-root, acyclic, fully-wired graph ----------
Object.keys(cases).forEach(function (name) {
  const g = S.astToGraph(cases[name]);
  const consumed = {};
  g.connections.forEach(function (c) {
    consumed[c.source] = true;
  });
  const roots = g.nodes.filter(function (n) {
    return !consumed[n.id];
  });
  ok(roots.length === 1, "single root: " + name);
});

// --- The operator vocabulary matches rules/ast.go opSpecs -------------------
const EXPECTED_OPS = [
  "eq", "ne", "lt", "le", "gt", "ge", "in", "nin",
  "has", "hasAll", "hasAny", "hasNone", "subsetOf", "hasKey",
  "isEmpty", "isNotEmpty", "exists",
  "before", "after", "onOrBefore", "onOrAfter", "between", "sameDay", "sameMonth", "sameYear",
];
ok(eq(S.OPS, EXPECTED_OPS), "OPS carries all 25 operators in ast.go order");
ok(
  eq(S.OPS.filter(S.isUnaryOp), ["isEmpty", "isNotEmpty", "exists"]),
  "exactly isEmpty/isNotEmpty/exists are unary"
);
ok(
  eq(S.OPS.filter(S.isDateOp), [
    "before", "after", "onOrBefore", "onOrAfter", "between", "sameDay", "sameMonth", "sameYear",
  ]),
  "exactly the eight date operators are date operators"
);
ok(S.OP_SPECS.between.right === S.RIGHT.BOUNDS, "between is the one operator taking RIGHT.BOUNDS");
ok(
  S.OPS.filter(function (op) {
    return S.OP_SPECS[op].right === S.RIGHT.BOUNDS;
  }).length === 1,
  "no operator but between takes RIGHT.BOUNDS"
);
["hasAll", "hasAny", "hasNone", "subsetOf", "isEmpty", "isNotEmpty"].forEach(function (fn) {
  ok(S.FUNCTIONS.indexOf(fn) >= 0, "FUNCTIONS advertises the collection backing " + fn);
});

// --- A unary compare emits NO `right` key, in either direction ---------------
["isEmpty", "isNotEmpty", "exists"].forEach(function (op) {
  const ast = { type: "compare", op: op, left: { type: "var", name: "object.tags" } };
  const back = S.graphToAST(S.astToGraph(ast));
  ok(!("right" in back), op + ": round-trip emits no `right` key");
  ok(JSON.stringify(back) === JSON.stringify(ast), op + ": round-trip is byte-identical");
  // ...and the editor never offers a right pin for one.
  ok(eq(S.inputKeys("compare", 0, op), ["left"]), op + ": inputKeys drops the right pin");
});
ok(eq(S.inputKeys("compare", 0, "eq"), ["left", "right"]), "a binary op keeps both pins");
ok(eq(S.inputKeys("compare", 0), ["left", "right"]), "inputKeys defaults to the binary shape");

// --- astToGraph refuses a unary op that carries a right operand -------------
try {
  S.astToGraph({
    type: "compare",
    op: "isEmpty",
    left: { type: "var", name: "object.tags" },
    right: { type: "literal", value: "x" },
  });
  ok(false, "unary op with a right operand should throw");
} catch (e) {
  ok(e.code === "APERTURE_RULE_INVALID", "unary op with a right operand throws APERTURE_RULE_INVALID");
}

// --- graphToAST refuses a `right` wire into a unary compare -----------------
try {
  S.graphToAST({
    nodes: [
      { id: "c", type: "compare", op: "exists" },
      { id: "l", type: "var", name: "object.owner" },
      { id: "r", type: "literal", value: 1 },
    ],
    connections: [
      { source: "l", sourceKey: "out", target: "c", targetKey: "left" },
      { source: "r", sourceKey: "out", target: "c", targetKey: "right" },
    ],
  });
  ok(false, "unary compare with a wired right input should throw");
} catch (e) {
  ok(e.code === "APERTURE_RULE_INVALID", "unary compare with a wired right input throws APERTURE_RULE_INVALID");
}

// --- Empty canvas -> null AST; null AST -> empty graph ----------------------
ok(S.graphToAST({ nodes: [], connections: [] }) === null, "empty graph -> null AST");
ok(eq(S.astToGraph(null), { nodes: [], connections: [] }), "null AST -> empty graph");

// --- validateAST catches the structural errors ast.go catches --------------
ok(S.validateAST(cases["nested and/or/not"]).length === 0, "valid AST has no problems");
ok(
  S.validateAST({ type: "and", children: [{ type: "literal", value: 1 }] }).some(function (p) {
    return p.code === "APERTURE_RULE_INVALID";
  }),
  "and with one child is invalid"
);
ok(
  S.validateAST({ type: "var", name: "bogus.field" }).some(function (p) {
    return p.code === "APERTURE_RULE_UNKNOWN_VARIABLE";
  }),
  "unknown variable root flagged"
);
ok(
  S.validateAST({
    type: "compare",
    op: "in",
    left: { type: "var", name: "object.x" },
    right: { type: "literal", value: "y" },
  }).some(function (p) {
    return p.code === "APERTURE_RULE_INVALID";
  }),
  "in with non-list/var right is invalid"
);
ok(
  S.validateAST({ type: "not", children: [{ type: "var", name: "object.a" }, { type: "var", name: "object.b" }] }).length > 0,
  "not with two children is invalid"
);

// --- Per-operator operand rules, mirroring ast.go validateCompare -----------

// Every representative AST above validates clean, unary ones included.
Object.keys(cases).forEach(function (name) {
  ok(S.validateAST(cases[name]).length === 0, "validates clean: " + name);
});

const listRight = { type: "list", items: [{ type: "literal", value: "a" }] };
const varRight = { type: "var", name: "object.other" };
const litRight = { type: "literal", value: "a" };
const leftVar = { type: "var", name: "object.tags" };

// Collection operators: a list or a var on the right, nothing else.
["in", "nin", "hasAll", "hasAny", "hasNone", "subsetOf"].forEach(function (op) {
  ok(S.validateAST({ type: "compare", op: op, left: leftVar, right: listRight }).length === 0, op + " accepts a list");
  ok(S.validateAST({ type: "compare", op: op, left: leftVar, right: varRight }).length === 0, op + " accepts a var");
  ok(
    S.validateAST({ type: "compare", op: op, left: leftVar, right: litRight }).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /list or variable on the right/.test(p.message);
    }),
    op + " rejects a scalar on the right"
  );
});

// Element operators: anything but a list on the right.
["has", "hasKey"].forEach(function (op) {
  ok(S.validateAST({ type: "compare", op: op, left: leftVar, right: litRight }).length === 0, op + " accepts an element");
  ok(
    S.validateAST({ type: "compare", op: op, left: leftVar, right: listRight }).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /single element on the right/.test(p.message);
    }),
    op + " rejects a list on the right"
  );
});

// Unary operators: `right` omitted, and rejected when present.
["isEmpty", "isNotEmpty", "exists"].forEach(function (op) {
  ok(S.validateAST({ type: "compare", op: op, left: leftVar }).length === 0, op + " validates with no right operand");
  ok(
    S.validateAST({ type: "compare", op: op, left: leftVar, right: litRight }).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /unary operator takes no right operand/.test(p.message);
    }),
    op + " rejects a right operand"
  );
  ok(
    S.validateAST({ type: "compare", op: op }).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /requires a left operand/.test(p.message);
    }),
    op + " still requires a left operand"
  );
});

// Binary operators still require a right operand.
ok(
  S.validateAST({ type: "compare", op: "eq", left: leftVar }).some(function (p) {
    return p.code === "APERTURE_RULE_INVALID" && /left and a right operand/.test(p.message);
  }),
  "a binary op without a right operand is invalid"
);

// Operand subtrees are walked even under a collection operator.
ok(
  S.validateAST({
    type: "compare",
    op: "hasAny",
    left: { type: "var", name: "bogus.tags" },
    right: listRight,
  }).some(function (p) {
    return p.code === "APERTURE_RULE_UNKNOWN_VARIABLE";
  }),
  "unknown variable under hasAny is still flagged"
);

// expr's predicate builtins are denied structurally, exactly as ast.go does.
S.BLOCKED_CALLS.forEach(function (name) {
  ok(
    S.validateAST({ type: "call", name: name, items: [{ type: "var", name: "object.tags" }] }).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /not callable from a rule/.test(p.message);
    }),
    "blocked builtin rejected: " + name
  );
});
ok(
  S.validateAST({ type: "call", name: "len", items: [{ type: "var", name: "object.tags" }] }).length === 0,
  "a curated function is still callable"
);

// --- Byte-for-byte against the Go contract test's own fixtures --------------
// These strings are COPIED VERBATIM from rules/editor_contract_test.go, which
// asserts each is byte-stable through rules.Node. Parsing them here, round-
// tripping through the graph, and re-serializing proves the two sides agree on
// key ORDER as well as on content — the half a deep-equal comparison misses, and
// exactly where the relativeDate quartet (type, anchor, n, unit, snap) could
// silently drift.
[
  '{"type":"compare","op":"onOrAfter","left":{"type":"var","name":"object.touched_at"},"right":{"type":"relativeDate","anchor":"NOW","n":-3,"unit":"months","snap":"none"}}',
  '{"type":"compare","op":"onOrAfter","left":{"type":"var","name":"object.touched_at"},"right":{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"startOfYear"}}',
  '{"type":"compare","op":"before","left":{"type":"var","name":"object.expires_at"},"right":{"type":"relativeDate","anchor":"NOW","n":12,"unit":"hours","snap":"none"}}',
  '{"type":"compare","op":"onOrBefore","left":{"type":"var","name":"object.due_at"},"right":{"type":"relativeDate","anchor":"TODAY","n":1,"unit":"quarters","snap":"endOfQuarter"}}',
  '{"type":"compare","op":"sameYear","left":{"type":"var","name":"object.hired_at"},"right":{"type":"relativeDate","anchor":"NOW","n":-1,"unit":"years","snap":"startOfWeek"}}',
  '{"type":"compare","op":"between","left":{"type":"var","name":"object.hired_at"},"right":{"type":"list","items":[{"type":"relativeDate","anchor":"NOW","n":-5,"unit":"years","snap":"startOfYear"},{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"endOfDay"}]}}',
  '{"type":"compare","op":"between","left":{"type":"var","name":"object.hired_at"},"right":{"type":"list","items":[{"type":"literal","value":"2026-01-01"},{"type":"relativeDate","anchor":"NOW","n":-30,"unit":"minutes","snap":"none"}]}}',
  '{"type":"and","children":[{"type":"compare","op":"eq","left":{"type":"var","name":"object.classification"},"right":{"type":"literal","value":"public"}},{"type":"compare","op":"after","left":{"type":"var","name":"object.touched_at"},"right":{"type":"relativeDate","anchor":"NOW","n":-90,"unit":"days","snap":"startOfDay"}}]}',
].forEach(function (src) {
  const ast = JSON.parse(src);
  ok(JSON.stringify(S.graphToAST(S.astToGraph(ast))) === src, "byte-identical round-trip: " + src.slice(0, 64) + "…");
  ok(S.validateAST(ast).length === 0, "Go fixture validates clean: " + src.slice(0, 64) + "…");
});

// --- The three relativeDate vocabularies (rules/relative.go) ----------------
// Membership is what the Go contract test compares (as a SET); the order below
// is this file's presentation choice and is asserted so it cannot be reshuffled
// silently under the editor's controls.
ok(eq(S.ANCHORS, ["NOW", "TODAY"]), "ANCHORS carries both anchors");
ok(
  eq(S.UNITS, ["years", "quarters", "months", "weeks", "days", "hours", "minutes"]),
  "UNITS carries all seven offset units, coarsest first"
);
ok(
  eq(S.SNAPS, [
    "none",
    "startOfYear", "endOfYear",
    "startOfQuarter", "endOfQuarter",
    "startOfMonth", "endOfMonth",
    "startOfWeek", "endOfWeek",
    "startOfDay", "endOfDay",
  ]),
  "SNAPS carries all eleven snaps, identity first then widest to narrowest"
);
ok(S.TYPES.RELATIVE_DATE === "relativeDate", "TYPES carries the relativeDate node type");
ok(!!S.NODE_SPECS.relativeDate, "NODE_SPECS carries a relativeDate entry");
ok(eq(S.inputKeys("relativeDate", 0), []), "a relative date is a leaf: no input pins");

// --- between: the ternary shape --------------------------------------------
const twoBounds = {
  type: "list",
  items: [{ type: "literal", value: "2026-01-01" }, { type: "literal", value: "2026-12-31" }],
};
ok(
  S.validateAST({ type: "compare", op: "between", left: leftVar, right: twoBounds }).length === 0,
  "between accepts exactly two bounds"
);
[
  ["one bound", { type: "list", items: [{ type: "literal", value: "2026-01-01" }] }],
  ["three bounds", { type: "list", items: [litRight, litRight, litRight] }],
  ["no bounds", { type: "list", items: [] }],
  ["a bare literal", litRight],
  ["a var", varRight],
].forEach(function (pair) {
  ok(
    S.validateAST({ type: "compare", op: "between", left: leftVar, right: pair[1] }).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /exactly two bounds on the right/.test(p.message);
    }),
    "between rejects " + pair[0]
  );
});
// An operand subtree under between is still walked, through either bound.
ok(
  S.validateAST({
    type: "compare",
    op: "between",
    left: leftVar,
    right: { type: "list", items: [{ type: "var", name: "bogus.lo" }, { type: "literal", value: "2026-12-31" }] },
  }).some(function (p) {
    return p.code === "APERTURE_RULE_UNKNOWN_VARIABLE";
  }),
  "an unknown variable in a between bound is still flagged"
);

// --- relativeDate: positional legality --------------------------------------
const relNow = { type: "relativeDate", anchor: "NOW", n: -3, unit: "months", snap: "none" };

// LEGAL: either operand of any date operator, and either between bound.
S.DATE_OPS.forEach(function (op) {
  const right = op === "between" ? { type: "list", items: [relNow, relNow] } : relNow;
  ok(
    S.validateAST({ type: "compare", op: op, left: leftVar, right: right }).length === 0,
    op + " accepts a relative date on the right"
  );
  const bin = op === "between" ? twoBounds : litRight;
  ok(
    S.validateAST({ type: "compare", op: op, left: relNow, right: bin }).length === 0,
    op + " accepts a relative date on the left"
  );
});

// ILLEGAL everywhere else. Each of these is a position the Go walk refuses.
[
  ["as the right of eq", { type: "compare", op: "eq", left: leftVar, right: relNow }],
  ["as the left of gt", { type: "compare", op: "gt", left: relNow, right: litRight }],
  ["inside an in list", { type: "compare", op: "in", left: leftVar, right: { type: "list", items: [relNow] } }],
  ["as a call argument", { type: "call", name: "lower", items: [relNow] }],
  ["as a logical child", { type: "and", children: [relNow, { type: "compare", op: "exists", left: leftVar }] }],
  ["as the whole rule", relNow],
  // `has` takes a single element on the right, so the SHAPE is fine — it is
  // date-ness, not shape, that refuses this one.
  ["as the element operand of has", { type: "compare", op: "has", left: leftVar, right: relNow }],
].forEach(function (pair) {
  ok(
    S.validateAST(pair[1]).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && /only valid as an operand of a date operator/.test(p.message);
    }),
    "a relative date is rejected " + pair[0]
  );
});
// The permission is NOT inherited: a relative date buried under a call that is
// itself a date operand is still refused.
ok(
  S.validateAST({
    type: "compare",
    op: "before",
    left: leftVar,
    right: { type: "call", name: "lower", items: [relNow] },
  }).some(function (p) {
    return /only valid as an operand of a date operator/.test(p.message);
  }),
  "the date-operand permission does not reach a nested call argument"
);

// --- relativeDate: field validation, one message per field ------------------
function relWith(patch) {
  const n = { type: "relativeDate", anchor: "NOW", n: -3, unit: "months", snap: "none" };
  Object.keys(patch).forEach(function (k) {
    n[k] = patch[k];
  });
  return { type: "compare", op: "before", left: leftVar, right: n };
}
[
  ["unknown anchor", { anchor: "YESTERDAY" }, /unknown anchor/],
  ["missing anchor", { anchor: undefined }, /unknown anchor/],
  ["lower-case anchor", { anchor: "now" }, /unknown anchor/],
  ["unknown unit", { unit: "fortnights" }, /unknown offset unit/],
  ["missing unit", { unit: undefined }, /unknown offset unit/],
  ["unknown snap", { snap: "startOfFiscalYear" }, /unknown snap/],
  ["missing snap (absence is not `none`)", { snap: undefined }, /unknown snap/],
  ["fractional offset", { n: 1.5 }, /whole number/],
  ["non-numeric offset", { n: "soon" }, /whole number/],
  ["missing offset", { n: undefined }, /whole number/],
  ["out-of-range offset", { n: 4000000000 }, /whole number/],
].forEach(function (tc) {
  ok(
    S.validateAST(relWith(tc[1])).some(function (p) {
      return p.code === "APERTURE_RULE_INVALID" && tc[2].test(p.message);
    }),
    "relative date rejects " + tc[0]
  );
});
// n: 0 and snap: "none" are the real spellings of "no offset" / "no snap".
ok(S.validateAST(relWith({ n: 0 })).length === 0, "n: 0 is a valid offset");
ok(S.validateAST(relWith({ n: "-3" })).length === 0, "a numeric-string offset is accepted");
// Each field fails INDEPENDENTLY, so all four controls can be flagged at once.
ok(
  S.validateAST(relWith({ anchor: "X", n: 1.5, unit: "Y", snap: "Z" })).length === 4,
  "all four relative-date fields report independently"
);

// --- normalizeOffset: the `n` control becomes a JSON number ------------------
function offsetOf(value) {
  return S.graphToAST({
    nodes: [{ id: "r", type: "relativeDate", anchor: "NOW", n: value, unit: "days", snap: "none" }],
    connections: [],
  }).n;
}
ok(offsetOf(-3) === -3, "a number offset passes through untouched");
ok(offsetOf("-3") === -3, "a text control's digits become a number");
ok(offsetOf("") === 0, "an empty offset control means no offset");
ok(offsetOf(undefined) === 0, "an unset offset control means no offset");
ok(offsetOf("soon") === "soon", "an unparseable offset survives verbatim for the validator");
ok(typeof offsetOf(" 12 ") === "number", "a padded numeric offset still normalizes");
// All four keys are emitted even when the controls are empty, so absence never
// silently means something.
const bareRel = S.graphToAST({ nodes: [{ id: "r", type: "relativeDate" }], connections: [] });
ok(
  eq(Object.keys(bareRel), ["type", "anchor", "n", "unit", "snap"]),
  "a relative date always emits all four keys, in the Go struct's order"
);

// --- graphToAST rejects a multi-root graph ----------------------------------
try {
  S.graphToAST({
    nodes: [
      { id: "a", type: "literal", value: 1 },
      { id: "b", type: "literal", value: 2 },
    ],
    connections: [],
  });
  ok(false, "multi-root graph should throw");
} catch (e) {
  ok(e.code === "APERTURE_RULE_INVALID", "multi-root graph throws APERTURE_RULE_INVALID");
}

// --- parseLiteral / formatLiteral preserve scalar types ---------------------
ok(S.parseLiteral("false") === false, "parseLiteral false");
ok(S.parseLiteral("0") === 0, "parseLiteral 0");
ok(S.parseLiteral('"hi"') === "hi", "parseLiteral quoted string");
ok(S.parseLiteral("null") === null, "parseLiteral null");
ok(S.parseLiteral("plain words") === "plain words", "parseLiteral bare text -> string");
ok(S.formatLiteral(false) === "false", "formatLiteral false");
ok(S.formatLiteral("hi") === "hi", "formatLiteral string unquoted");


// --- Read-back: a date rule returns as the rule that was authored ------------
//
// The round-trip invariant above is deep-equality over a fixed case list. What
// follows is the AUTHOR'S version of it, driven from the operator table so it
// cannot fall behind: every date operator, byte-identical, key order included.
//
// Byte-identity (not deep-equality) is the assertion that matters here. The
// relativeDate quartet is four adjacent optional keys, and a serializer that
// emitted them in a different order — or emitted `n` as a string — would still
// deep-equal on some comparisons while producing a rule that no longer matches
// what Go marshals.

// dateCaseFor builds one authored rule per operator: a relative-date operand on
// the right, and for the ternary operator a mixed pair of bounds (a literal and
// a relative date), which is the shape an author reaches for most often.
function dateCaseFor(op) {
  const left = { type: "var", name: "object.hired_at" };
  const rel = { type: "relativeDate", anchor: "TODAY", n: -2, unit: "quarters", snap: "endOfMonth" };
  if (S.RIGHT && (S.OP_SPECS[op] || {}).right === S.RIGHT.BOUNDS) {
    return {
      type: "compare",
      op: op,
      left: left,
      right: { type: "list", items: [{ type: "literal", value: "2026-01-01" }, rel] },
    };
  }
  return { type: "compare", op: op, left: left, right: rel };
}

ok(S.DATE_OPS.length === 8, "DATE_OPS carries the eight date operators (got " + S.DATE_OPS.length + ")");
S.DATE_OPS.forEach(function (op) {
  const ast = dateCaseFor(op);
  const src = JSON.stringify(ast);
  const back = JSON.stringify(S.graphToAST(S.astToGraph(ast)));
  ok(back === src, "authored " + op + " reads back byte-identical");
  ok(S.validateAST(ast).length === 0, "authored " + op + " validates clean");
});

// A `between` authored as ONE node reads back as ONE node. The AST story chose
// the two-item-list shape over desugaring to and(onOrAfter, onOrBefore) exactly
// so this holds without either side re-sugaring a two-child `and` heuristically:
// there is nothing to re-sugar, so the proof is that the shape survives.
const authoredBetween = dateCaseFor("between");
const betweenBack = S.graphToAST(S.astToGraph(authoredBetween));
ok(betweenBack.type === "compare" && betweenBack.op === "between", "between reads back as a single compare node");
ok(
  betweenBack.right.type === "list" && betweenBack.right.items.length === 2,
  "between's bounds read back as one two-item list"
);
ok(
  JSON.stringify(betweenBack).indexOf('"and"') < 0 && JSON.stringify(betweenBack).indexOf("onOrBefore") < 0,
  "between is not desugared into and(onOrAfter, onOrBefore)"
);

// TODAY reads back as TODAY. It is a distinct persisted anchor, NOT sugar for
// NOW + snap:startOfDay — an author who chose it must get it back, and a snap
// they chose on top of it must survive alongside it rather than being folded in.
const todayAst = {
  type: "compare",
  op: "after",
  left: { type: "var", name: "object.hired_at" },
  right: { type: "relativeDate", anchor: "TODAY", n: -1, unit: "weeks", snap: "startOfWeek" },
};
const todayBack = S.graphToAST(S.astToGraph(todayAst));
ok(todayBack.right.anchor === "TODAY", "TODAY reads back as TODAY, not as NOW");
ok(todayBack.right.snap === "startOfWeek", "a snap chosen on top of TODAY survives the round-trip");
ok(JSON.stringify(todayBack) === JSON.stringify(todayAst), "the TODAY rule is byte-identical");

// The graph an AST loads into carries the four relativeDate fields VERBATIM as
// node properties — they are the four controls, so this is what "the same
// controls come back" means at the serializer boundary.
const loadedRel = S.astToGraph(todayAst).nodes.filter(function (n) {
  return n.type === "relativeDate";
})[0];
ok(
  loadedRel && loadedRel.anchor === "TODAY" && loadedRel.n === -1 &&
    loadedRel.unit === "weeks" && loadedRel.snap === "startOfWeek",
  "a loaded relative date carries its four control values verbatim"
);

// An unparseable offset survives the load so the validator can name it, rather
// than the editor silently choosing a number the author did not write.
const oddOffset = {
  type: "compare",
  op: "before",
  left: { type: "var", name: "object.hired_at" },
  right: { type: "relativeDate", anchor: "NOW", n: 1.5, unit: "days", snap: "none" },
};
ok(
  S.astToGraph(oddOffset).nodes.filter(function (n) { return n.type === "relativeDate"; })[0].n === 1.5,
  "a non-integer offset loads verbatim instead of being coerced"
);
ok(
  S.validateAST(oddOffset).some(function (p) {
    return /relative date offset must be a whole number/.test(p.message);
  }),
  "and is reported by the validator rather than by the control"
);

// --- Leaf layout: two relative bounds do not overlap on load ----------------
// Layout is cosmetic (graphToAST drops positions), but a `between` with two
// relative bounds is the common case and the two nodes are four controls tall,
// so a single row pitch stacked them on top of each other until the author
// dragged them apart. The assertion is DERIVED, not a pixel constant: a pair of
// tall leaves must be spaced further apart than a pair of short ones.
function boundGap(lo, hi) {
  const g = S.astToGraph({
    type: "compare",
    op: "between",
    left: { type: "var", name: "object.hired_at" },
    right: { type: "list", items: [lo, hi] },
  });
  const ys = g.nodes.filter(function (n) {
    return n.type === lo.type;
  }).map(function (n) {
    return n.position.y;
  });
  return Math.abs(ys[1] - ys[0]);
}
const relBound = { type: "relativeDate", anchor: "NOW", n: -1, unit: "years", snap: "startOfYear" };
const litBound = { type: "literal", value: "2026-01-01" };
ok(boundGap(relBound, relBound) > boundGap(litBound, litBound), "two relative bounds load further apart than two literals");
ok(boundGap(relBound, relBound) > 0, "two relative bounds do not load on top of each other");

// --- The editor's label layer is an exact inverse ---------------------------
//
// rules.js is a CLASSIC script: it assigns window.rules at load and exports
// nothing, so it cannot be require()d. It CAN be evaluated in a vm context with
// a window shim, which makes its top-level declarations reachable — and that is
// worth doing, because the label layer is the one part of the read-back the
// serializer does not cover.
//
// What it guards: the controls display readable spellings ("start of quarter",
// "is on or before") while the AST stores tokens, so a rule loaded from the
// server passes through vocabLabel on the way in and vocabFromLabel on the way
// out. If those two are not exact inverses over the closed vocabularies, a rule
// that is saved, reloaded and saved again is a DIFFERENT rule — silently, and
// only for the members where they disagree.
(function editorLabelRoundTrip() {
  let editor;
  try {
    const fs = require("fs");
    const vm = require("vm");
    const src = fs.readFileSync(__dirname + "/rules.js", "utf8");
    editor = { window: { RuleSerializer: S }, console: console };
    editor.globalThis = editor;
    vm.runInNewContext(src, editor, { filename: "rules.js" });
  } catch (e) {
    ok(false, "rules.js evaluates in a vm context with a window shim: " + e.message);
    return;
  }
  ok(typeof editor.vocabLabel === "function", "rules.js exposes its label helpers to the vm context");

  [["ANCHORS", S.ANCHORS], ["UNITS", S.UNITS], ["SNAPS", S.SNAPS]].forEach(function (pair) {
    pair[1].forEach(function (token) {
      const shown = editor.vocabLabel(token);
      ok(
        editor.vocabFromLabel(pair[1], shown) === token,
        pair[0] + ": " + token + ' displays as "' + shown + '" and reads back as itself'
      );
    });
  });
  Object.keys(S.OP_SPECS).forEach(function (op) {
    ok(editor.opFromLabel(editor.opLabel(op)) === op, "operator " + op + " reads back through its spelling");
  });
  // A token the author typed that is in no vocabulary passes through VERBATIM,
  // so the validator names what they wrote instead of the editor substituting
  // something plausible.
  ok(editor.vocabFromLabel(S.UNITS, "fortnights") === "fortnights", "an unknown unit passes through verbatim");
  ok(editor.opFromLabel("is nearly before") === "is nearly before", "an unknown operator spelling passes through verbatim");
  // The offset seed keeps a number as a number and a malformed token as itself;
  // only a blank becomes 0.
  ok(editor.offsetInitial(-3) === -3, "offsetInitial keeps a number");
  ok(editor.offsetInitial("") === 0, "offsetInitial turns a blank into 0");
  ok(editor.offsetInitial(undefined) === 0, "offsetInitial turns an absent offset into 0");
  ok(editor.offsetInitial("1.5") === "1.5", "offsetInitial keeps a malformed token verbatim");
})();

if (failures > 0) {
  console.error("\n" + failures + " failure(s)");
  process.exit(1);
}
console.log("\nAll serializer round-trip tests passed.");
