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
];
ok(eq(S.OPS, EXPECTED_OPS), "OPS carries all 17 operators in ast.go order");
ok(
  eq(S.OPS.filter(S.isUnaryOp), ["isEmpty", "isNotEmpty", "exists"]),
  "exactly isEmpty/isNotEmpty/exists are unary"
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

if (failures > 0) {
  console.error("\n" + failures + " failure(s)");
  process.exit(1);
}
console.log("\nAll serializer round-trip tests passed.");
