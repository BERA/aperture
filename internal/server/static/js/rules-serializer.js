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
 * children, items, and the relativeDate quartet anchor, n, unit, snap; all
 * others omitted, including `right` on the three UNARY comparison operators).
 * The Rete UI (rules.js) is only an editing surface that produces/consumes this
 * plain graph model.
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
    // A date expressed RELATIVE to the decision's reference instant: an anchor,
    // a whole-number offset in one of seven units, and a snap. It is an OPERAND,
    // never a predicate — legal only on either side of a date operator (either
    // `between` bound included), and rejected anywhere else. See ANCHORS/UNITS/
    // SNAPS and validateRelativeDate below.
    RELATIVE_DATE: "relativeDate",
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
  //   BOUNDS      a `list` holding EXACTLY TWO operand nodes — the ternary
  //               shape, used only by `between`. It is deliberately NOT a new
  //               field or a new node type: `right` stays one node, so every
  //               rule persisted before dates existed is byte-identical, and the
  //               author's one `between` node reads back as one node rather than
  //               as a re-sugared pair of comparisons
  const RIGHT = {
    ANY: "any",
    COLLECTION: "collection",
    ELEMENT: "element",
    NONE: "none",
    BOUNDS: "bounds",
  };

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
  //
  // The last eight are the DATE operators. Seven of them reuse RIGHT.ELEMENT —
  // the shape rule constrains AST STRUCTURE (one value, never a set), and
  // date-ness is a runtime question the server answers deny-safely — so a
  // relativeDate operand needed no shape of its own. `between` is the exception
  // and takes RIGHT.BOUNDS.
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

    before: { right: RIGHT.ELEMENT }, // object.hired_at before "2026-01-01"       — strict
    after: { right: RIGHT.ELEMENT }, // object.hired_at after "2026-01-01"        — strict
    onOrBefore: { right: RIGHT.ELEMENT }, // on or before                              — inclusive
    onOrAfter: { right: RIGHT.ELEMENT }, // on or after                               — inclusive
    between: { right: RIGHT.BOUNDS }, // between ["2026-01-01", "2026-12-31"]      — inclusive both ends
    sameDay: { right: RIGHT.ELEMENT }, // same UTC calendar day
    sameMonth: { right: RIGHT.ELEMENT }, // same UTC calendar month
    sameYear: { right: RIGHT.ELEMENT }, // same UTC calendar year
  };

  // DATE_OPS is the subset of OP_SPECS that compares DATES — the operators whose
  // Go opSpec carries the renderDate strategy. It is a separate list rather than
  // a flag on the OP_SPECS entries because those entries are machine-read by the
  // Go contract test in the literal `{ right: RIGHT.X }` form; a second key there
  // would break the scanner. The gate compares this list against the Go table
  // instead, so the two cannot drift either.
  //
  // It exists because date-ness is a POSITIONAL permission, not a shape: a
  // relativeDate operand is legal only under one of these operators, so
  // validateAST has to ask "is this a date operator?" and the palette has to know
  // which operators offer date controls.
  const DATE_OPS = [
    "before",
    "after",
    "onOrBefore",
    "onOrAfter",
    "between",
    "sameDay",
    "sameMonth",
    "sameYear",
  ];

  // isDateOp reports whether `op` compares dates — i.e. whether a relativeDate is
  // legal as one of its operands.
  function isDateOp(op) {
    return DATE_OPS.indexOf(op) >= 0;
  }

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

  // ---- The relativeDate vocabularies (rules/relative.go) --------------------
  //
  // Three closed sets, one per field of a relativeDate node. Each drives one
  // editor control AND one branch of validateRelativeDate, so a member the
  // editor does not offer is a rule an author cannot write, and a member the
  // editor offers but the server does not know is a rule that fails on save.
  // Both directions are drift, so the Go contract test compares both.
  //
  // The Go side holds each as a MAP (membership is the only question it asks),
  // and compares these AS A SET — so the order below is purely PRESENTATION, and
  // it is chosen here, once, for the editor's controls to read in.
  //
  // NEVER build a JS Date out of any of this. The engine is UTC end to end and
  // the editor renders stored date strings verbatim, `Z` included: a viewer in
  // UTC-5 shown `toLocaleString()` would read a stored 2026-01-01T00:00:00Z as
  // "2025-12-31 19:00" — a different calendar YEAR. That is a correctness rule,
  // not a preference.

  // ANCHORS — the point a relative date is measured from. TODAY is NOW snapped to
  // the start of its UTC day; it is a DISTINCT persisted anchor, never rewritten
  // into NOW + snap: startOfDay, and it still takes a snap on top.
  const ANCHORS = ["NOW", "TODAY"];

  // UNITS — the seven offset units, coarsest first, which is how an author scans
  // for one. The first three are CALENDAR units and clamp at month end on the
  // server; the last four are fixed-length.
  const UNITS = ["years", "quarters", "months", "weeks", "days", "hours", "minutes"];

  // SNAPS — the calendar boundary the anchor is rounded to BEFORE the offset is
  // applied. `none` leads because it is the identity and the control's usual
  // value; the rest are widest-to-narrowest, start before end within each period,
  // so the pairs read together. An `end*` boundary is the LAST representable
  // instant of its period (23:59:59), not the start of the next one.
  const SNAPS = [
    "none",
    "startOfYear",
    "endOfYear",
    "startOfQuarter",
    "endOfQuarter",
    "startOfMonth",
    "endOfMonth",
    "startOfWeek",
    "endOfWeek",
    "startOfDay",
    "endOfDay",
  ];

  // OFFSET_LIMIT is the magnitude bound on a relativeDate's `n`. The Go validator
  // reads the field with a 32-bit signed parse, so anything outside this range is
  // "not a whole number" there and must be here too.
  const OFFSET_MIN = -2147483648;
  const OFFSET_MAX = 2147483647;

  // JSON_NUMBER matches the JSON number grammar exactly — the token form the Go
  // side stores the offset in (a json.Number, not an int, precisely so the
  // VALIDATOR and not the JSON decoder owns rejecting `1.5`).
  const JSON_NUMBER = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/;

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
    // A relative date is a LEAF operand: it produces a value and takes no wires.
    // Its four fields are controls (anchor, n, unit, snap), not input pins —
    // every one of them is a closed-set token or a whole number, so there is
    // nothing another node could legally feed it.
    relativeDate: { title: "Relative date", category: "operand", out: SOCKET.VALUE, inputs: "none", accepts: [] },
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
        case TYPES.RELATIVE_DATE:
          // Key order matters, exactly as it does for a compare: the emitted
          // object is compared against the Go marshalling, whose struct order is
          // type, anchor, n, unit, snap.
          //
          // ALL FOUR KEYS ARE ALWAYS EMITTED. "No offset" is n: 0 and "no snap"
          // is the vocabulary member "none" — absence never means anything, so
          // an empty control is a validation problem rather than a silently
          // different rule.
          return {
            type: TYPES.RELATIVE_DATE,
            anchor: n.anchor === undefined || n.anchor === null ? "" : String(n.anchor),
            n: normalizeOffset(n.n),
            unit: n.unit === undefined || n.unit === null ? "" : String(n.unit),
            snap: n.snap === undefined || n.snap === null ? "" : String(n.snap),
          };
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

  // normalizeOffset turns whatever a relativeDate's `n` control holds into the
  // JSON NUMBER the AST stores. A number passes through untouched, so a loaded
  // rule round-trips byte-for-byte; an empty control is 0 ("no offset"); and a
  // token that is not a JSON number is returned VERBATIM rather than coerced, so
  // validateRelativeDate can report the Go validator's own wording instead of the
  // editor silently inventing a cutoff the author did not write.
  function normalizeOffset(value) {
    if (typeof value === "number") return value;
    if (value === null || value === undefined) return 0;
    const s = String(value).trim();
    if (s === "") return 0;
    if (!JSON_NUMBER.test(s)) return s;
    return Number(s);
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
    // Leaf rows advance by the HEIGHT of the leaf just placed, not by a fixed
    // index, because the leaves are not all the same height: a var or a literal
    // is one control tall, while a relativeDate carries four. With a single row
    // pitch, two relative bounds loaded from an AST — which is exactly what a
    // `between` with two of them is — overlapped until the author dragged them
    // apart or hit "Fit to view".
    //
    // Layout is cosmetic and is NOT part of the AST: graphToAST drops positions
    // entirely, so nothing here can change what a rule means or how it
    // serializes. The numbers are approximate node heights plus a gap; the
    // canvas is free-form and an author moves anything they like.
    const ROW = 96;
    const TALL_ROW = 200;

    // leafRow reports the vertical space one leaf needs.
    function leafRow(type) {
      return type === TYPES.RELATIVE_DATE ? TALL_ROW : ROW;
    }

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
        case TYPES.RELATIVE_DATE:
          // A leaf: the four fields become the node's four controls, carried
          // verbatim so graphToAST can hand them straight back.
          g.anchor = node.anchor;
          g.n = node.n;
          g.unit = node.unit;
          g.snap = node.snap;
          break;
        default:
          throw serErr("APERTURE_RULE_INVALID", "unknown node type: " + String(node.type));
      }

      // Position: a parent sits at the average y of its children; a leaf takes
      // the next free row and advances the cursor by its own height. Cosmetic
      // only — positions are dropped by graphToAST.
      if (childYs.length > 0) {
        g.position.y = childYs.reduce(function (a, b) { return a + b; }, 0) / childYs.length;
      } else {
        g.position.y = leafCursor;
        leafCursor += leafRow(node.type);
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
    // `dateOperand` is the POSITIONAL permission a relativeDate needs: true only
    // when this node sits directly under a date operator (either side, or either
    // `between` bound). It is deliberately NOT inherited — validateCompare hands
    // it to its own operands, and every recursion below passes false — so a
    // relativeDate buried inside a call argument or a list under a date operator
    // is refused exactly as the Go walk refuses it.
    function walk(n, path, dateOperand) {
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
        case TYPES.RELATIVE_DATE:
          if (!dateOperand) {
            // It resolves to a date the author never sees, so anywhere it is not
            // being compared AS a date it would either be string-compared against
            // a value that changes every second, or silently deny. Refusing it
            // here makes the mistake visible when the rule is SAVED.
            report(
              "APERTURE_RULE_INVALID",
              "a relative date is only valid as an operand of a date operator: " + String(n.anchor),
              path
            );
            return;
          }
          validateRelativeDate(n, path, report);
          break;
        default:
          report("APERTURE_RULE_INVALID", "unknown node type: " + String(n.type), path);
      }
    }
    walk(ast, "$", false);
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
  //
  // A DATE operator additionally grants its own operands — and, for `between`,
  // each of its two bounds independently — permission to be a relativeDate. The
  // flag rides along rather than being re-derived per operand, so the two sides
  // of one comparison cannot disagree about it.
  function validateCompare(n, path, report, walk) {
    let spec = OP_SPECS[n.op];
    if (!spec) {
      report("APERTURE_RULE_INVALID", "unknown comparison operator: " + String(n.op), path);
      // Fall back to the loosest shape so the operand subtrees are still walked:
      // an unknown operator should not hide a bad variable underneath it. The
      // server rejects the node either way.
      spec = { right: RIGHT.ANY };
    }
    const dateOp = isDateOp(n.op);
    const left = n.left;
    const right = n.right;
    if (left === undefined || left === null) {
      report("APERTURE_RULE_INVALID", "comparison requires a left operand: " + String(n.op), path);
    }

    if (spec.right === RIGHT.NONE) {
      if (right !== undefined && right !== null) {
        report("APERTURE_RULE_INVALID", "unary operator takes no right operand: " + String(n.op), path + ".right");
      }
      // No unary operator is a date operator, so the left operand of one is not a
      // date-operand position; `dateOp` is false here by construction.
      if (left !== undefined && left !== null) walk(left, path + ".left", dateOp);
      return;
    }

    if (right === undefined || right === null) {
      report("APERTURE_RULE_INVALID", "comparison requires a left and a right operand", path);
      if (left !== undefined && left !== null) walk(left, path + ".left", dateOp);
      return;
    }

    if (spec.right === RIGHT.BOUNDS) {
      // The ternary shape: exactly two bounds, in a list. Both failure modes
      // share one message — an author who supplied the wrong container and one
      // who supplied the wrong number of bounds need the same correction.
      //
      // The bounds are walked as OPERANDS in their own right, NOT through the
      // list node, because that is what carries the date-operand permission down
      // to them: each bound may independently be a literal, a variable, or a
      // relativeDate.
      if (right.type !== TYPES.LIST || !Array.isArray(right.items) || right.items.length !== 2) {
        report(
          "APERTURE_RULE_INVALID",
          "operator requires a list of exactly two bounds on the right: " + String(n.op),
          path + ".right"
        );
      }
      if (left !== undefined && left !== null) walk(left, path + ".left", dateOp);
      if (right.type === TYPES.LIST) {
        (right.items || []).forEach(function (bound, i) {
          walk(bound, path + ".right.items[" + i + "]", dateOp);
        });
      } else {
        walk(right, path + ".right", dateOp);
      }
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
    if (left !== undefined && left !== null) walk(left, path + ".left", dateOp);
    walk(right, path + ".right", dateOp);
  }

  // validateRelativeDate mirrors `validateRelativeDate` in rules/relative.go: the
  // node's four fields are each checked against their closed set.
  //
  // Each field reports its OWN problem rather than short-circuiting on the first,
  // because each names a different mistake with a different fix — an anchor the
  // author invented, an offset that is not a whole number, a unit that does not
  // exist, a snap that does not exist — and the editor renders each message
  // beside the control it belongs to.
  function validateRelativeDate(n, path, report) {
    if (ANCHORS.indexOf(n.anchor) < 0) {
      report("APERTURE_RULE_INVALID", "relative date has an unknown anchor: " + String(n.anchor), path);
    }
    if (!isWholeOffset(n.n)) {
      report("APERTURE_RULE_INVALID", "relative date offset must be a whole number: " + String(n.n), path);
    }
    if (UNITS.indexOf(n.unit) < 0) {
      report("APERTURE_RULE_INVALID", "relative date has an unknown offset unit: " + String(n.unit), path);
    }
    if (SNAPS.indexOf(n.snap) < 0) {
      report("APERTURE_RULE_INVALID", "relative date has an unknown snap: " + String(n.snap), path);
    }
  }

  // isWholeOffset mirrors the Go side's 32-bit base-10 integer parse of the `n`
  // field: a fraction, an out-of-range magnitude, a non-numeric token, and an
  // absent field are all the same correction — write a whole number — and so are
  // all one problem with one wording.
  //
  // A number that arrived through JSON.parse has already lost its original token
  // (`1e3` parses to 1000), which is the one place this can be laxer than Go; the
  // editor's own control never produces that form.
  function isWholeOffset(value) {
    if (typeof value === "string") {
      return /^-?\d+$/.test(value.trim()) && isWholeOffset(Number(value.trim()));
    }
    if (typeof value !== "number") return false;
    return Number.isInteger(value) && value >= OFFSET_MIN && value <= OFFSET_MAX;
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
    DATE_OPS: DATE_OPS,
    ROOTS: ROOTS,
    FUNCTIONS: FUNCTIONS,
    BLOCKED_CALLS: BLOCKED_CALLS,
    ANCHORS: ANCHORS,
    UNITS: UNITS,
    SNAPS: SNAPS,
    SOCKET: SOCKET,
    NODE_SPECS: NODE_SPECS,
    VAR_PATH: VAR_PATH,
    isUnaryOp: isUnaryOp,
    isDateOp: isDateOp,
    inputKeys: inputKeys,
    graphToAST: graphToAST,
    astToGraph: astToGraph,
    validateAST: validateAST,
    parseLiteral: parseLiteral,
    formatLiteral: formatLiteral,
  };
});
