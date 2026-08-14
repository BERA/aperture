/*
 * rules-serializer.js — the load-bearing graph <-> rule-AST bridge (E7-S2).
 *
 * PURE, DOM-FREE, DEPENDENCY-FREE. This module knows nothing about Rete, React
 * or Alpine. It defines a plain graph model (nodes with typed data + directed
 * connections) and two pure functions:
 *
 *   graphToAST(graph)  -> rule AST node (the E2-S3 `rules.Node` JSON shape)
 *   astToGraph(ast)    -> plain graph { nodes, connections }
 *
 * The pair round-trips LOSSLESSLY against the E2-S3 AST: for any valid AST,
 *   graphToAST(astToGraph(ast))  deep-equals  ast.
 * There is NO second rule format — the AST here is byte-for-byte the same shape
 * `rules/ast.go` marshals (fields: type, op, name, value, left, right,
 * children, items; all others omitted, including `right` on the three UNARY
 * comparison operators). The Rete UI (rules.js) is only an editing surface that
 * produces/consumes this plain graph model.
 *
 * Because it is pure it is unit-testable under `node` with no browser — see
 * rules-serializer.test.js (run: `node internal/server/static/js/rules-serializer.test.js`).
 *
 * The module is UMD-ish: it attaches `window.RuleSerializer` in the browser and
 * exports the same object under CommonJS so the node test can require() it.
 */
(function (root, factory) {
  const api = factory();
  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (typeof window !== "undefined") {
    window.RuleSerializer = api;
  }
})(this, function () {
  "use strict";

  // ---- AST vocabulary (mirrors rules/ast.go exactly) -----------------------

  // Node types — the closed discriminator set. Kept identical to NodeType in
  // rules/ast.go so the editor palette maps one-to-one.
  const TYPES = {
    AND: "and",
    OR: "or",
    NOT: "not",
    COMPARE: "compare",
    VAR: "var",
    LITERAL: "literal",
    LIST: "list",
    CALL: "call",
  };

  // RIGHT is the shape a comparison's right operand must take — the JS mirror of
  // `rightShape` in ast.go, and the operand-shape half of an operator's
  // contract:
  //
  //   ANY         any operand node (the scalar comparisons)
  //   COLLECTION  a `list` literal or a `var`: the right operand is itself a
  //               collection (in, nin, hasAll, hasAny, hasNone, subsetOf)
  //   ELEMENT     a single element — anything but a `list`. A set on the right
  //               is hasAll/hasAny/hasNone, not has (has, hasKey)
  //   NONE        no right operand at all: the unary operators, whose compare
  //               node carries NO `right` key
  const RIGHT = { ANY: "any", COLLECTION: "collection", ELEMENT: "element", NONE: "none" };

  // OP_SPECS is the closed comparison-operator registry, mirroring `opSpecs` in
  // rules/ast.go one entry for one entry. Go keeps the operand shape and the
  // render strategy in a single table so Validate and render cannot disagree.
  // This side needs only the shape — rendering to expr-lang is the server's job
  // — but it keeps the SAME table structure so the two files stay comparable at
  // a glance, and so validateAST is a table lookup rather than a pile of per-op
  // conditionals that drift one operator at a time.
  //
  // The first eight are the scalar/membership operators; the nine that follow
  // are the collection operators. Three of those (isEmpty, isNotEmpty, exists)
  // are UNARY: they reuse a `compare` node with `right` OMITTED rather than
  // introducing a new node type, so the editor gains nine operators and no new
  // palette primitive. OMITTING the key — never emitting `right: null` — is what
  // keeps the AST byte-identical across a round-trip through the editor.
  const OP_SPECS = {
    eq: { right: RIGHT.ANY }, // ==
    ne: { right: RIGHT.ANY }, // !=
    lt: { right: RIGHT.ANY }, // <
    le: { right: RIGHT.ANY }, // <=
    gt: { right: RIGHT.ANY }, // >
    ge: { right: RIGHT.ANY }, // >=
    in: { right: RIGHT.COLLECTION }, // object.region in ["us", "eu"]
    nin: { right: RIGHT.COLLECTION }, // not in

    has: { right: RIGHT.ELEMENT }, // object.tags has "x"          (array)
    hasAll: { right: RIGHT.COLLECTION }, // object.tags has all [a, b]  (array)
    hasAny: { right: RIGHT.COLLECTION }, // object.tags has any [a, b]  (array)
    hasNone: { right: RIGHT.COLLECTION }, // object.tags has none [a, b] (array)
    subsetOf: { right: RIGHT.COLLECTION }, // object.tags subset of [a,b] (array)
    hasKey: { right: RIGHT.ELEMENT }, // object.owner has key "dept"  (object)
    isEmpty: { right: RIGHT.NONE }, // object.tags is empty         — unary
    isNotEmpty: { right: RIGHT.NONE }, // object.tags is not empty     — unary
    exists: { right: RIGHT.NONE }, // object.owner.dept exists     — unary
  };

  // Comparison operators carried in a compare node's `op`, derived from the one
  // registry above so the list can never fall behind it. Key insertion order is
  // preserved, so this reads in the same order as the Op* consts in ast.go.
  const OPS = Object.keys(OP_SPECS);

  // isUnaryOp reports whether `op` takes NO right operand. Both serializer
  // directions consult it: graphToAST must not emit a `right` key for one of
  // these, and astToGraph must not wire a `right` input for one.
  function isUnaryOp(op) {
    const spec = OP_SPECS[op];
    return !!spec && spec.right === RIGHT.NONE;
  }

  // Context-variable roots a `var` may reference (allowedRoots in ast.go).
  const ROOTS = ["object", "principal", "account", "action"];

  // The default pure functions a `call` may name (rules/compiler.go
  // defaultFunctions). Advisory only for the palette — the server is the
  // authority (a host may register more), so validateAST does NOT reject an
  // unknown function name; it only checks the identifier shape, like ast.go.
  //
  // The last six back the collection operators, which have no native expr-lang
  // spelling once builtins are disabled. They are callable by name as well as
  // reachable through their operator, exactly as on the Go side.
  const FUNCTIONS = [
    "lower",
    "upper",
    "contains",
    "startsWith",
    "endsWith",
    "len",
    "hasAll",
    "hasAny",
    "hasNone",
    "subsetOf",
    "isEmpty",
    "isNotEmpty",
  ];

  // BLOCKED_CALLS mirrors `blockedCallNames` in ast.go: expr-lang's PREDICATE
  // builtins, which survive expr.DisableAllBuiltins because the parser matches a
  // predicate name before it consults the disabled-builtin table. Nothing this
  // AST emits reaches them today, but a `call` node renders `name(args...)`
  // verbatim, so the Go validator denies the names structurally. The editor's
  // save path denies the same names, so a rule the server would refuse is
  // refused here first — before the round-trip to the API.
  const BLOCKED_CALLS = [
    "all", "none", "any", "one", "filter", "map", "count", "sum", "find",
    "findIndex", "findLast", "findLastIndex", "groupBy", "sortBy", "reduce",
  ];

  // Dotted-identifier-path matcher (varPath in ast.go). Each segment is a
  // Go-style identifier, which keeps the rendered expression injection-free.
  const VAR_PATH = /^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$/;

  // ---- Node specs: the single source of truth for pins the UI + serializer
  // share. `inputs` names the arity shape; the serializer derives connection
  // target keys from it, and the Rete UI builds the matching input sockets.
  //
  //   out      the socket TYPE a node's single output produces
  //   inputs   'none' | 'child' | 'leftright' | 'variadic'
  //   accepts  the socket type(s) an input pin accepts (for typed sockets)
  const SOCKET = { BOOL: "bool", VALUE: "value", LIST: "list" };

  const NODE_SPECS = {
    and: { title: "And", category: "logic", out: SOCKET.BOOL, inputs: "variadic", accepts: [SOCKET.BOOL] },
    or: { title: "Or", category: "logic", out: SOCKET.BOOL, inputs: "variadic", accepts: [SOCKET.BOOL] },
    not: { title: "Not", category: "logic", out: SOCKET.BOOL, inputs: "child", accepts: [SOCKET.BOOL] },
    compare: { title: "Compare", category: "compare", out: SOCKET.BOOL, inputs: "leftright", accepts: [SOCKET.VALUE, SOCKET.LIST] },
    var: { title: "Variable", category: "operand", out: SOCKET.VALUE, inputs: "none", accepts: [] },
    literal: { title: "Literal", category: "operand", out: SOCKET.VALUE, inputs: "none", accepts: [] },
    list: { title: "List", category: "pulse", out: SOCKET.LIST, inputs: "variadic", accepts: [SOCKET.VALUE] },
    call: { title: "Call", category: "pulse", out: SOCKET.VALUE, inputs: "variadic", accepts: [SOCKET.VALUE, SOCKET.LIST] },
  };

  // inputKeys returns the ordered input-socket keys a node of `type` exposes
  // given how many variadic slots it currently has. The keys are the contract
  // between the graph model and both serializer directions.
  //
  // `op` is consulted only for a compare node, and only to drop the `right` pin
  // for the three unary operators — a unary compare has one operand, so offering
  // a second pin would invite a wire the AST has nowhere to put. It is optional
  // and defaults to the binary shape, so callers that predate the collection
  // operators keep their behaviour.
  function inputKeys(type, arity, op) {
    const spec = NODE_SPECS[type];
    if (!spec) return [];
    switch (spec.inputs) {
      case "none":
        return [];
      case "child":
        return ["in"];
      case "leftright":
        return isUnaryOp(op) ? ["left"] : ["left", "right"];
      case "variadic": {
        const n = Math.max(0, arity || 0);
        const keys = [];
        for (let i = 0; i < n; i++) keys.push("in-" + i);
        return keys;
      }
      default:
        return [];
    }
  }

  // ---- graph -> AST --------------------------------------------------------

  // graphToAST folds a plain graph into a rule AST. It finds the single root
  // (the node whose output feeds no input) and recurses. Throws a structured
  // error ({ code, message }) if the graph is not a single connected tree — the
  // editor surfaces that through validate() before any save.
  function graphToAST(graph) {
    const g = graph || {};
    const nodes = {};
    (g.nodes || []).forEach(function (n) {
      nodes[n.id] = n;
    });

    // incoming[targetId] = { key: sourceId }; consumed[sourceId] = true.
    const incoming = {};
    const consumed = {};
    (g.connections || []).forEach(function (c) {
      if (!incoming[c.target]) incoming[c.target] = {};
      incoming[c.target][c.targetKey] = c.source;
      consumed[c.source] = true;
    });

    const ids = Object.keys(nodes);
    if (ids.length === 0) {
      return null; // empty canvas -> no rule
    }
    const roots = ids.filter(function (id) {
      return !consumed[id];
    });
    if (roots.length !== 1) {
      throw serErr(
        "APERTURE_RULE_INVALID",
        "graph must have exactly one root node (found " + roots.length + ")"
      );
    }

    const seen = {};
    function build(id) {
      const n = nodes[id];
      if (!n) throw serErr("APERTURE_RULE_INVALID", "connection references a missing node: " + id);
      if (seen[id]) throw serErr("APERTURE_RULE_INVALID", "graph has a cycle at node " + id);
      seen[id] = true;
      const inc = incoming[id] || {};
      const type = n.type;
      switch (type) {
        case TYPES.AND:
        case TYPES.OR:
          return { type: type, children: orderedSources(inc).map(build) };
        case TYPES.NOT:
          return { type: TYPES.NOT, children: [build(requireSource(inc, "in", id))] };
        case TYPES.COMPARE: {
          // Key order matters: the emitted object is compared against the Go
          // marshalling (type, op, left, right), so `right` is assigned last.
          const op = n.op || "";
          const cmp = { type: TYPES.COMPARE, op: op, left: build(requireSource(inc, "left", id)) };
          if (isUnaryOp(op)) {
            // A unary compare emits NO `right` key at all. Setting it to null
            // would round-trip as a literal null on the Go side and break the
            // byte-identical guarantee, so a stray wire into `right` is an
            // error rather than something silently dropped.
            if (inc.right !== undefined) {
              throw serErr(
                "APERTURE_RULE_INVALID",
                "node " + id + " uses the unary operator `" + op + "`, which takes no right operand"
              );
            }
            return cmp;
          }
          cmp.right = build(requireSource(inc, "right", id));
          return cmp;
        }
        case TYPES.VAR:
          return { type: TYPES.VAR, name: n.name || "" };
        case TYPES.LITERAL:
          // Always emit `value`, even when falsy (false/0/""/null): those are
          // non-empty RawMessage on the Go side and must survive the round-trip.
          return { type: TYPES.LITERAL, value: n.value === undefined ? null : n.value };
        case TYPES.LIST:
          return { type: TYPES.LIST, items: orderedSources(inc).map(build) };
        case TYPES.CALL:
          return { type: TYPES.CALL, name: n.name || "", items: orderedSources(inc).map(build) };
        default:
          throw serErr("APERTURE_RULE_INVALID", "unknown node type: " + String(type));
      }
    }
    return build(roots[0]);
  }

  // orderedSources returns the sources of `in-0`, `in-1`, ... in numeric order.
  function orderedSources(inc) {
    return Object.keys(inc)
      .filter(function (k) {
        return /^in-\d+$/.test(k);
      })
      .sort(function (a, b) {
        return parseInt(a.slice(3), 10) - parseInt(b.slice(3), 10);
      })
      .map(function (k) {
        return inc[k];
      });
  }

  function requireSource(inc, key, id) {
    const s = inc[key];
    if (s === undefined) {
      throw serErr("APERTURE_RULE_INVALID", "node " + id + " is missing its `" + key + "` input");
    }
    return s;
  }

  // ---- AST -> graph --------------------------------------------------------

  // astToGraph expands an AST into a plain graph the editor can render. Node ids
  // and positions are editor concerns (NOT part of the AST) and are dropped when
  // going back the other way, so the round-trip stays lossless. Layout is a
  // simple left-to-right layered placement (x by depth, y by leaf order) — purely
  // cosmetic; pan/zoom/reroute in the canvas can move anything afterwards.
  function astToGraph(ast) {
    const nodes = [];
    const connections = [];
    let counter = 0;
    let leafCursor = 0;
    const nextId = function () {
      return "n" + ++counter;
    };
    const COL = 260;
    const ROW = 96;

    function walk(node, depth) {
      const id = nextId();
      const g = { id: id, type: node.type, position: { x: depth * COL, y: 0 } };
      let childYs = [];

      function wire(childNode, targetKey) {
        const childId = walk(childNode, depth + 1);
        connections.push({ source: childId, sourceKey: "out", target: id, targetKey: targetKey });
        const cn = nodeById(nodes, childId);
        if (cn) childYs.push(cn.position.y);
        return childId;
      }

      switch (node.type) {
        case TYPES.AND:
        case TYPES.OR:
          (node.children || []).forEach(function (ch, i) {
            wire(ch, "in-" + i);
          });
          break;
        case TYPES.NOT:
          wire((node.children || [])[0], "in");
          break;
        case TYPES.COMPARE: {
          g.op = node.op;
          wire(node.left, "left");
          // The unary operators have no right operand, so no `right` pin is
          // wired — which is exactly what makes graphToAST omit the key on the
          // way back. A `right` present on a unary op is a malformed AST (the
          // Go Validate rejects it too) and is reported rather than dropped,
          // because dropping it would silently change the rule.
          const hasRight = node.right !== undefined && node.right !== null;
          if (isUnaryOp(node.op)) {
            if (hasRight) {
              throw serErr(
                "APERTURE_RULE_INVALID",
                "unary operator takes no right operand: " + String(node.op)
              );
            }
            break;
          }
          if (!hasRight) {
            throw serErr("APERTURE_RULE_INVALID", "comparison requires a left and a right operand");
          }
          wire(node.right, "right");
          break;
        }
        case TYPES.VAR:
          g.name = node.name;
          break;
        case TYPES.LITERAL:
          g.value = node.value === undefined ? null : node.value;
          break;
        case TYPES.LIST:
          (node.items || []).forEach(function (it, i) {
            wire(it, "in-" + i);
          });
          break;
        case TYPES.CALL:
          g.name = node.name;
          (node.items || []).forEach(function (it, i) {
            wire(it, "in-" + i);
          });
          break;
        default:
          throw serErr("APERTURE_RULE_INVALID", "unknown node type: " + String(node.type));
      }

      // Position: a parent sits at the average y of its children; a leaf takes
      // the next free row. Cosmetic only.
      if (childYs.length > 0) {
        g.position.y = childYs.reduce(function (a, b) { return a + b; }, 0) / childYs.length;
      } else {
        g.position.y = leafCursor * ROW;
        leafCursor++;
      }
      nodes.push(g);
      return id;
    }

    if (ast) walk(ast, 0);
    return { nodes: nodes, connections: connections };
  }

  function nodeById(nodes, id) {
    for (let i = 0; i < nodes.length; i++) {
      if (nodes[i].id === id) return nodes[i];
    }
    return null;
  }

  // ---- client-side structural validation -----------------------------------

  // validateAST mirrors rules/ast.go Validate: it checks AST SHAPE only, not
  // types. Full type-checking (operand types, function arity/signatures) is the
  // engine's job and runs server-side in E7-S3 — this is the fast, offline
  // pre-flight that catches malformed graphs and unknown-variable roots before a
  // save round-trips to the API. Returns an array of { code, message, path }.
  function validateAST(ast) {
    const problems = [];
    function report(code, message, path) {
      problems.push({ code: code, message: message, path: path });
    }
    function walk(n, path) {
      if (n === null || n === undefined) {
        report("APERTURE_RULE_INVALID", "nil node", path);
        return;
      }
      switch (n.type) {
        case TYPES.AND:
        case TYPES.OR:
          if (!Array.isArray(n.children) || n.children.length < 2) {
            report("APERTURE_RULE_INVALID", n.type + " requires at least two children", path);
          }
          (n.children || []).forEach(function (c, i) {
            walk(c, path + ".children[" + i + "]");
          });
          break;
        case TYPES.NOT:
          if (!Array.isArray(n.children) || n.children.length !== 1) {
            report("APERTURE_RULE_INVALID", "not requires exactly one child", path);
          }
          (n.children || []).forEach(function (c, i) {
            walk(c, path + ".children[" + i + "]");
          });
          break;
        case TYPES.COMPARE:
          validateCompare(n, path, report, walk);
          break;
        case TYPES.VAR:
          validateVar(n.name, path, report);
          break;
        case TYPES.LITERAL:
          validateLiteral(n.value, path, report);
          break;
        case TYPES.LIST:
          (n.items || []).forEach(function (it, i) {
            walk(it, path + ".items[" + i + "]");
          });
          break;
        case TYPES.CALL:
          if (!n.name || !VAR_PATH.test(n.name)) {
            report("APERTURE_RULE_INVALID", "call has an invalid function name: " + String(n.name), path);
          } else if (BLOCKED_CALLS.indexOf(n.name) >= 0) {
            // expr's predicate builtins survive DisableAllBuiltins, so the Go
            // validator denies them by name; deny them here too or the editor
            // would happily save a rule the server refuses.
            report("APERTURE_RULE_INVALID", "function is not callable from a rule: " + String(n.name), path);
          }
          (n.items || []).forEach(function (it, i) {
            walk(it, path + ".items[" + i + "]");
          });
          break;
        default:
          report("APERTURE_RULE_INVALID", "unknown node type: " + String(n.type), path);
      }
    }
    walk(ast, "$");
    return problems;
  }

  // validateCompare mirrors `validateCompare` in ast.go: the operator must be
  // known, the operand ARITY must match it (a unary operator takes no right
  // operand; every other one requires both), and the right operand's SHAPE must
  // be something the operator can act on. Driving all three off OP_SPECS is what
  // keeps this in step with the Go table.
  //
  // The arity rule is deliberately per-operator rather than blanket: exactly
  // isEmpty / isNotEmpty / exists must omit `right`, and a `right` supplied for
  // one of them is an ERROR rather than something silently ignored — otherwise
  // the AST would have two spellings for the same rule and would stop
  // round-tripping predictably.
  //
  // Messages and codes are the Go validator's, so a rule refused on the canvas
  // reports what the server would have reported. Where the Go error carries an
  // `op` context field, this appends it after a colon, matching the convention
  // the rest of validateAST already uses.
  function validateCompare(n, path, report, walk) {
    let spec = OP_SPECS[n.op];
    if (!spec) {
      report("APERTURE_RULE_INVALID", "unknown comparison operator: " + String(n.op), path);
      // Fall back to the loosest shape so the operand subtrees are still walked:
      // an unknown operator should not hide a bad variable underneath it. The
      // server rejects the node either way.
      spec = { right: RIGHT.ANY };
    }
    const left = n.left;
    const right = n.right;
    if (left === undefined || left === null) {
      report("APERTURE_RULE_INVALID", "comparison requires a left operand: " + String(n.op), path);
    }

    if (spec.right === RIGHT.NONE) {
      if (right !== undefined && right !== null) {
        report("APERTURE_RULE_INVALID", "unary operator takes no right operand: " + String(n.op), path + ".right");
      }
      if (left !== undefined && left !== null) walk(left, path + ".left");
      return;
    }

    if (right === undefined || right === null) {
      report("APERTURE_RULE_INVALID", "comparison requires a left and a right operand", path);
      if (left !== undefined && left !== null) walk(left, path + ".left");
      return;
    }

    if (spec.right === RIGHT.COLLECTION && right.type !== TYPES.LIST && right.type !== TYPES.VAR) {
      report(
        "APERTURE_RULE_INVALID",
        "operator requires a list or variable on the right: " + String(n.op),
        path + ".right"
      );
    }
    if (spec.right === RIGHT.ELEMENT && right.type === TYPES.LIST) {
      report(
        "APERTURE_RULE_INVALID",
        "operator requires a single element on the right; use hasAll/hasAny/hasNone for a set: " + String(n.op),
        path + ".right"
      );
    }
    if (left !== undefined && left !== null) walk(left, path + ".left");
    walk(right, path + ".right");
  }

  function validateVar(name, path, report) {
    if (!name || !VAR_PATH.test(name)) {
      report("APERTURE_RULE_INVALID", "variable reference is not a dotted identifier path: " + String(name), path);
      return;
    }
    const root = name.indexOf(".") >= 0 ? name.slice(0, name.indexOf(".")) : name;
    if (ROOTS.indexOf(root) < 0) {
      report("APERTURE_RULE_UNKNOWN_VARIABLE", "variable root is not an exposed context root: " + root, path);
    }
  }

  function validateLiteral(value, path, report) {
    if (value === undefined) {
      report("APERTURE_RULE_INVALID", "literal has no value", path);
      return;
    }
    const t = typeof value;
    if (value !== null && t !== "boolean" && t !== "number" && t !== "string") {
      report("APERTURE_RULE_INVALID", "literal must be a scalar (string, number, bool, or null); use a list node for collections", path);
    }
  }

  // parseLiteral turns the text a literal control holds into a scalar JS value,
  // preserving type: true/false/null, JSON numbers, and JSON strings parse to
  // their value; any other bare text is taken as a string. This is what lets
  // false/0/""/null survive as real scalars rather than the word "false".
  function parseLiteral(text) {
    if (text === null || text === undefined) return null;
    const s = String(text).trim();
    if (s === "") return "";
    try {
      const v = JSON.parse(s);
      const t = typeof v;
      if (v === null || t === "boolean" || t === "number" || t === "string") {
        return v;
      }
    } catch (_) {
      /* fall through: treat as a plain string */
    }
    return s;
  }

  // formatLiteral is the inverse of parseLiteral for display in a text control:
  // strings show unquoted, everything else shows its JSON form.
  function formatLiteral(value) {
    if (typeof value === "string") return value;
    if (value === undefined) return "";
    return JSON.stringify(value);
  }

  function serErr(code, message) {
    const e = new Error(message);
    e.code = code;
    return e;
  }

  return {
    TYPES: TYPES,
    OPS: OPS,
    OP_SPECS: OP_SPECS,
    RIGHT: RIGHT,
    ROOTS: ROOTS,
    FUNCTIONS: FUNCTIONS,
    BLOCKED_CALLS: BLOCKED_CALLS,
    SOCKET: SOCKET,
    NODE_SPECS: NODE_SPECS,
    VAR_PATH: VAR_PATH,
    isUnaryOp: isUnaryOp,
    inputKeys: inputKeys,
    graphToAST: graphToAST,
    astToGraph: astToGraph,
    validateAST: validateAST,
    parseLiteral: parseLiteral,
    formatLiteral: formatLiteral,
  };
});
