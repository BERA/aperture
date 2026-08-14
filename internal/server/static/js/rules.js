/*
 * rules.js — the Rules section: a Blueprints-style node editor over the E2-S3
 * pulse-expression rule AST (E7-S2).
 *
 * This replaces the E7-S1 hello-canvas. It builds the real node<->AST editor on
 * the toolkit the vendored Rete bundle re-exports (NodeEditor, ClassicPreset,
 * AreaPlugin, AreaExtensions, ConnectionPlugin/Presets, ReactPlugin/Presets),
 * imported at runtime from /vendor/rete/rete.min.js (no node build in dev/CI).
 *
 * The editor is ONLY an editing surface. All graph<->AST correctness lives in
 * the pure, DOM-free serializer (rules-serializer.js, window.RuleSerializer):
 * the Rete graph is read into that module's plain graph model and folded to the
 * exact rules.Node JSON — there is no second rule format.
 *
 * Alpine holds the section state and exposes the E7-S3 save/load HOOKS on
 * `window.blueprintEditor`:
 *
 *   window.blueprintEditor.toAST()        -> rule AST (rules.Node JSON) | null
 *   window.blueprintEditor.fromAST(ast)   -> Promise (render an AST into the graph)
 *   window.blueprintEditor.validate()     -> [{ code, message, path }]  (client-side)
 *   window.blueprintEditor.getGraph()     -> plain { nodes, connections }
 *   window.blueprintEditor.addNode(kind, data?) -> Promise (palette add; `data`
 *                                         seeds the node's controls, e.g.
 *                                         { op: "hasAll" } for a comparison)
 *   window.blueprintEditor.clear()        -> Promise
 *   window.blueprintEditor.destroy()
 *   window.blueprintEditor.onChange = fn  (set by the host; fires on any edit)
 *
 * E7-S3 wires POST-to-API load/save/validate over these hooks; the split is:
 * client-side validate() is structural/AST-shape only (mirrors ast.go Validate);
 * full type-checking is the engine's and runs server-side.
 */

/*
 * ---- Operator spellings ----------------------------------------------------
 *
 * The AST stores an operator as its TOKEN (`hasAll`, `isNotEmpty`, …) — that is
 * what `rules/ast.go` marshals and what the serializer's OP_SPECS is keyed by.
 * Those tokens are terrible to read on a canvas, so the editor shows a readable
 * SPELLING ("has all", "is not empty") everywhere an author sees an operator:
 * the palette entry and the comparison node's own operator control. The spelling
 * is presentation only — it is resolved back to the token the moment the graph
 * is read (reteToGraph), so nothing downstream ever sees it.
 *
 * OP_LABELS is a spelling table, NOT a second operator list: membership and
 * ORDER both come from the serializer's OP_SPECS (the mirror of Go's `opSpecs`),
 * so an operator added there shows up in the editor with no edit here — labelled
 * by its bare token until someone gives it a spelling. That is deliberate: the
 * operator set is already written down twice (Go + the serializer) and a third
 * hand-maintained copy is exactly how the three drift apart.
 */
const OP_LABELS = {
  eq: "equals",
  ne: "not equals",
  lt: "less than",
  le: "less or equal",
  gt: "greater than",
  ge: "greater or equal",
  in: "in",
  nin: "not in",
  has: "has member",
  hasAll: "has all",
  hasAny: "has any",
  hasNone: "has none",
  subsetOf: "subset of",
  hasKey: "has key",
  isEmpty: "is empty",
  isNotEmpty: "is not empty",
  exists: "exists",
};

// opSpecs returns the serializer's operator registry, or an empty table when the
// serializer has not loaded. Every reader below tolerates the empty case so a
// missing script degrades to an editor without operator affordances rather than
// a component that throws while Alpine is initialising it (mount() reports the
// missing serializer properly).
function opSpecs() {
  const S = typeof window !== "undefined" ? window.RuleSerializer : null;
  return (S && S.OP_SPECS) || {};
}

// opLabel renders an operator token as its readable spelling.
function opLabel(op) {
  const token = String(op === undefined || op === null ? "" : op);
  return OP_LABELS[token] || token;
}

// opFromLabel is the inverse: it maps a readable spelling back to its AST token.
// A raw token is accepted as-is (so a hand-typed `hasAll` still works), matching
// is whitespace- and case-insensitive, and anything unrecognised is returned
// VERBATIM so the validator reports "unknown comparison operator: <what you
// typed>" rather than this silently substituting something plausible.
function opFromLabel(text) {
  const raw = String(text === undefined || text === null ? "" : text).trim();
  const specs = opSpecs();
  if (Object.prototype.hasOwnProperty.call(specs, raw)) return raw;
  const norm = raw.toLowerCase().replace(/\s+/g, " ");
  const tokens = Object.keys(specs);
  for (let i = 0; i < tokens.length; i++) {
    if (opLabel(tokens[i]).toLowerCase() === norm) return tokens[i];
  }
  return raw;
}

// opListSeq numbers the per-editor <datalist> so two mounted editors (the shell
// re-creates the section on navigation) never share an element id.
let opListSeq = 0;

// operatorEntries lists every comparison operator once, in OP_SPECS order (which
// is ast.go's Op* order: the scalar/membership operators, then the collection
// ones). It is the single source both the palette and the node's operator
// dropdown are built from.
function operatorEntries() {
  return Object.keys(opSpecs()).map(function (op) {
    return { op: op, label: opLabel(op) };
  });
}

// createBlueprintEditor mounts the Rete editor into `container` and returns the
// blueprintEditor hook object. `mod` is the vendored Rete bundle namespace and
// `S` is window.RuleSerializer.
async function createBlueprintEditor(container, mod, S) {
  const {
    NodeEditor,
    ClassicPreset,
    AreaPlugin,
    AreaExtensions,
    ConnectionPlugin,
    ConnectionPresets,
    ReactPlugin,
    ReactPresets,
  } = mod;

  const editor = new NodeEditor();
  const area = new AreaPlugin(container);
  const connection = new ConnectionPlugin();
  const render = new ReactPlugin({ createRoot: mod.createRoot || undefined });

  // The bundle re-exports React's createRoot indirectly through ReactPlugin's
  // own default when not supplied; pass it if the bundle exposed it.
  render.addPreset(ReactPresets.classic.setup());
  connection.addPreset(ConnectionPresets.classic.setup());

  editor.use(area);
  area.use(connection);
  area.use(render);

  AreaExtensions.simpleNodesOrder(area);
  AreaExtensions.selectableNodes(area, AreaExtensions.selector(), {
    accumulating: AreaExtensions.accumulateOnCtrl(),
  });

  // One shared socket instance: Rete's default compatibility then permits any
  // wire, and the typed-socket rules are enforced by the connectioncreate guard
  // below using the serializer's NODE_SPECS. Keeping one socket avoids the
  // classic preset silently refusing visually-identical pins.
  const socket = new ClassicPreset.Socket("pin");

  const hook = {
    editor,
    area,
    onChange: function () {},
  };

  // fireChange notifies the host (Alpine) that the graph changed so it can
  // re-run validation. Debounced to a microtask so a burst of Rete events
  // (e.g. clearing) collapses into one refresh.
  let pending = false;
  function fireChange() {
    if (pending) return;
    pending = true;
    Promise.resolve().then(function () {
      pending = false;
      try {
        hook.onChange();
      } catch (_) {
        /* host errors must not break the editor pipeline */
      }
    });
  }

  // Typed-socket guard + change notifications. Returning undefined from a
  // 'connectioncreate' cancels the connection (Rete pipe contract).
  editor.addPipe(function (context) {
    if (!context || typeof context !== "object") return context;
    if (context.type === "connectioncreate") {
      if (!isCompatible(context.data)) {
        return; // reject incompatible or self-referential wires
      }
    }
    if (
      context.type === "connectioncreated" ||
      context.type === "connectionremoved" ||
      context.type === "noderemoved" ||
      context.type === "nodecreated"
    ) {
      fireChange();
    }
    return context;
  });

  // isCompatible enforces the typed-socket rules from NODE_SPECS: a source
  // node's output type must be accepted by the target input, and a node may not
  // wire into itself.
  function isCompatible(data) {
    const src = editor.getNode(data.source);
    const tgt = editor.getNode(data.target);
    if (!src || !tgt) return false;
    if (src.id === tgt.id) return false;
    const outType = (S.NODE_SPECS[src.kind] || {}).out;
    const accepts = (S.NODE_SPECS[tgt.kind] || {}).accepts || [];
    return accepts.indexOf(outType) >= 0;
  }

  // makeNode builds a Rete node for an AST kind, seeding its controls from
  // optional `data` (op/name/value) and its variadic pins from `data.arity`.
  function makeNode(kind, data) {
    const spec = S.NODE_SPECS[kind];
    if (!spec) throw new Error("unknown node kind: " + kind);
    const d = data || {};
    const node = new ClassicPreset.Node(spec.title);
    node.kind = kind;

    node.addOutput("out", new ClassicPreset.Output(socket, spec.out));

    if (kind === S.TYPES.COMPARE) {
      // `node.op` is the resolved AST TOKEN; the control shows the readable
      // spelling. Keeping the token on the node means the pin-shape logic never
      // has to re-parse the control's text.
      node.op = opFromLabel(d.op || "eq");
      node.addControl(
        "op",
        new ClassicPreset.InputControl("text", {
          initial: opLabel(node.op),
          change: function (v) {
            onOpChange(node, v);
          },
        })
      );
    }
    if (kind === S.TYPES.VAR) {
      node.addControl(
        "name",
        new ClassicPreset.InputControl("text", {
          initial: d.name || "object.",
          change: fireChange,
        })
      );
    }
    if (kind === S.TYPES.LITERAL) {
      node.addControl(
        "value",
        new ClassicPreset.InputControl("text", {
          initial: S.formatLiteral(d.value === undefined ? "" : d.value),
          change: fireChange,
        })
      );
    }
    if (kind === S.TYPES.CALL) {
      node.addControl(
        "name",
        new ClassicPreset.InputControl("text", {
          initial: d.name || "len",
          change: fireChange,
        })
      );
    }

    // Inputs. A comparison's pins come from the serializer's inputKeys() rather
    // than a hardcoded left/right pair: that is what makes the three unary
    // operators (isEmpty / isNotEmpty / exists) render as SINGLE-input nodes.
    // The `right` pin is never created for them, so there is no unusable socket
    // to wire and no wire for graphToAST to reject.
    if (spec.inputs === "child") {
      node.addInput("in", new ClassicPreset.Input(socket, ""));
    } else if (spec.inputs === "leftright") {
      S.inputKeys(kind, 0, node.op).forEach(function (key) {
        node.addInput(key, new ClassicPreset.Input(socket, key));
      });
    } else if (spec.inputs === "variadic") {
      const arity = Math.max(minArity(kind), d.arity || 0);
      node.arity = 0;
      for (let i = 0; i < arity; i++) {
        node.addInput("in-" + i, new ClassicPreset.Input(socket, "in " + i));
        node.arity++;
      }
      // A number control drives how many ordered input pins the node exposes;
      // order is meaningful for list/call arguments.
      node.addControl(
        "slots",
        new ClassicPreset.InputControl("number", {
          initial: node.arity,
          change: function (v) {
            adjustArity(node, v);
          },
        })
      );
    }
    return node;
  }

  function minArity(kind) {
    if (kind === S.TYPES.AND || kind === S.TYPES.OR) return 2;
    if (kind === S.TYPES.LIST) return 1;
    return 1; // call: allow zero-arg is possible, but keep one slot for editing
  }

  // adjustArity grows/shrinks a variadic node's ordered input pins, removing any
  // connections that fall off when shrinking.
  async function adjustArity(node, next) {
    next = Math.max(minArity(node.kind), Math.floor(next || 0));
    const cur = node.arity;
    if (next === cur) return;
    if (next > cur) {
      for (let i = cur; i < next; i++) {
        node.addInput("in-" + i, new ClassicPreset.Input(socket, "in " + i));
      }
    } else {
      for (let i = cur; i > next; i--) {
        const key = "in-" + (i - 1);
        const drop = editor.getConnections().filter(function (c) {
          return c.target === node.id && c.targetInput === key;
        });
        for (const c of drop) {
          await editor.removeConnection(c.id);
        }
        node.removeInput(key);
      }
    }
    node.arity = next;
    await area.update("node", node.id);
    fireChange();
  }

  // onOpChange runs on every edit of a comparison node's operator control. It
  // resolves the readable spelling back to the AST token and re-shapes the
  // node's pins, so the canvas matches the operator's ARITY the moment it is
  // chosen — switching to `is empty` collapses the node to one input, switching
  // back to a binary operator restores the second.
  function onOpChange(node, text) {
    node.op = opFromLabel(text);
    syncCompareInputs(node, node.op);
  }

  // syncCompareInputs serialises the re-shape per node. The control fires on
  // every keystroke, and applyCompareInputs awaits (connection removal, area
  // update), so two edits in flight could otherwise interleave and add the same
  // pin twice. Chaining on the node keeps them strictly ordered; a rejected link
  // must not stall the chain, hence the same handler for both outcomes.
  function syncCompareInputs(node, op) {
    const apply = function () {
      return applyCompareInputs(node, op);
    };
    node.opSync = (node.opSync || Promise.resolve()).then(apply, apply);
    return node.opSync;
  }

  // applyCompareInputs makes the node's pin set equal inputKeys() for `op`. It
  // is a diff, not a rebuild: pins that survive keep their identity and their
  // wires, so switching operators among the binary ones disturbs nothing. A pin
  // that goes away has its incoming connection REMOVED first — leaving it would
  // strand a wire pointing at a socket that no longer exists, which is the
  // dangling-`right` bug this replaces.
  async function applyCompareInputs(node, op) {
    const want = S.inputKeys(node.kind, node.arity || 0, op);
    const have = Object.keys(node.inputs || {});
    const drop = have.filter(function (k) {
      return want.indexOf(k) < 0;
    });
    const add = want.filter(function (k) {
      return have.indexOf(k) < 0;
    });
    if (drop.length === 0 && add.length === 0) {
      fireChange(); // the operator still changed, even if its arity did not
      return;
    }
    for (const key of drop) {
      const stale = editor.getConnections().filter(function (c) {
        return c.target === node.id && c.targetInput === key;
      });
      for (const c of stale) {
        await editor.removeConnection(c.id);
      }
      node.removeInput(key);
    }
    for (const key of add) {
      node.addInput(key, new ClassicPreset.Input(socket, key));
    }
    await area.update("node", node.id);
    fireChange();
  }

  /*
   * ---- The operator dropdown ----------------------------------------------
   *
   * Rete's classic control set is text/number only, and the React renderer that
   * draws the nodes is a COMMITTED VENDORED BLOB (vendor/rete/rete.min.js) that
   * this repo never rebuilds — so there is no custom <select> control to
   * register without a node build. The operator control is therefore a text
   * input bound to a native <datalist>: the browser turns it into a dropdown of
   * every operator's readable spelling, typing narrows it, and the field still
   * accepts a raw token. No framework, no dependency, no vendor change.
   *
   * The list is attached by DELEGATION rather than at node-build time because
   * the input element belongs to the renderer: it is created (and re-created) by
   * React, out of reach of the node model. Tagging it on pointerdown/focusin —
   * before the browser opens the list — is idempotent and survives any re-render.
   */
  const opListId = "a-rule-ops-" + ++opListSeq;
  const opList = document.createElement("datalist");
  opList.id = opListId;
  operatorEntries().forEach(function (entry) {
    const option = document.createElement("option");
    option.value = entry.label;
    opList.appendChild(option);
  });
  container.appendChild(opList);

  function attachOpList(ev) {
    const el = ev.target;
    if (!el || el.tagName !== "INPUT") return;
    if (el.getAttribute("list") === opListId) return;
    if (!isCompareControl(el)) return;
    el.setAttribute("list", opListId);
  }

  // isCompareControl reports whether `el` is the control of a comparison node.
  // A compare node carries exactly one control (the operator), so its node view
  // containing the input is enough to identify it — no renderer internals are
  // relied on beyond the area's node views, and a miss is a silent no-op.
  function isCompareControl(el) {
    try {
      const nodes = editor.getNodes();
      for (let i = 0; i < nodes.length; i++) {
        if (nodes[i].kind !== S.TYPES.COMPARE) continue;
        const view = area.nodeViews.get(nodes[i].id);
        if (view && view.element && view.element.contains(el)) return true;
      }
    } catch (_) {
      /* best-effort affordance: never break editing over it */
    }
    return false;
  }

  container.addEventListener("pointerdown", attachOpList, true);
  container.addEventListener("focusin", attachOpList, true);

  // reteToGraph reads the live Rete editor into the serializer's plain graph
  // model — the ONLY bridge between the runtime and the pure serializer.
  function reteToGraph() {
    const nodes = editor.getNodes().map(function (n) {
      const g = { id: n.id, type: n.kind };
      // The control holds the readable spelling; the AST wants the token. An
      // unrecognised spelling passes through verbatim so the validator can name
      // exactly what the author typed.
      if (n.controls.op) g.op = opFromLabel(n.controls.op.value);
      if (n.kind === S.TYPES.VAR || n.kind === S.TYPES.CALL) {
        g.name = n.controls.name ? n.controls.name.value : "";
      }
      if (n.kind === S.TYPES.LITERAL) {
        g.value = S.parseLiteral(n.controls.value ? n.controls.value.value : "");
      }
      return g;
    });
    const connections = editor.getConnections().map(function (c) {
      return {
        source: c.source,
        sourceKey: c.sourceOutput,
        target: c.target,
        targetKey: c.targetInput,
      };
    });
    return { nodes: nodes, connections: connections };
  }

  async function clear() {
    for (const c of editor.getConnections().slice()) {
      await editor.removeConnection(c.id);
    }
    for (const n of editor.getNodes().slice()) {
      await editor.removeNode(n.id);
    }
  }

  // fromAST renders an AST into the canvas. Node ids/positions are editor
  // concerns produced by astToGraph and are dropped on the way back, so the
  // round-trip stays lossless.
  async function fromAST(ast) {
    await clear();
    const graph = S.astToGraph(ast);

    // Per-node variadic arity = count of its in-N connections.
    const arity = {};
    graph.connections.forEach(function (c) {
      if (/^in-\d+$/.test(c.targetKey)) {
        arity[c.target] = (arity[c.target] || 0) + 1;
      }
    });

    const idMap = {};
    for (const gn of graph.nodes) {
      const node = makeNode(gn.type, {
        op: gn.op,
        name: gn.name,
        value: gn.value,
        arity: arity[gn.id] || 0,
      });
      idMap[gn.id] = node;
      await editor.addNode(node);
      if (gn.position) {
        await area.translate(node.id, gn.position);
      }
    }
    for (const c of graph.connections) {
      const src = idMap[c.source];
      const tgt = idMap[c.target];
      if (!src || !tgt) continue;
      await editor.addConnection(
        new ClassicPreset.Connection(src, c.sourceKey, tgt, c.targetKey)
      );
    }
    await zoomToFit();
    fireChange();
  }

  // addNode drops a fresh palette node onto the canvas near the origin, staggered
  // so successive adds do not stack exactly. `data` seeds the node's controls —
  // the operator palette passes { op: … } so the node arrives already set to the
  // operator that was clicked, with the pin set that operator's arity demands.
  let addOffset = 0;
  async function addNode(kind, data) {
    const node = makeNode(kind, data || {});
    await editor.addNode(node);
    const x = 40 + (addOffset % 5) * 30;
    const y = 40 + (addOffset % 5) * 30;
    addOffset++;
    await area.translate(node.id, { x: x, y: y });
    return node;
  }

  async function zoomToFit() {
    const nodes = editor.getNodes();
    if (nodes.length > 0) {
      await AreaExtensions.zoomAt(area, nodes);
    }
  }

  // toAST folds the live graph to the rule AST via the pure serializer. Throws a
  // structured error ({code, message}) if the graph is not a single tree.
  hook.toAST = function () {
    return S.graphToAST(reteToGraph());
  };
  hook.fromAST = fromAST;
  hook.getGraph = reteToGraph;
  hook.addNode = addNode;
  hook.clear = clear;
  hook.zoomToFit = zoomToFit;
  hook.validate = function () {
    let ast;
    try {
      ast = S.graphToAST(reteToGraph());
    } catch (e) {
      return [{ code: e.code || "APERTURE_RULE_INVALID", message: e.message, path: "$" }];
    }
    if (ast === null) return [];
    return S.validateAST(ast);
  };
  hook.destroy = function () {
    container.removeEventListener("pointerdown", attachOpList, true);
    container.removeEventListener("focusin", attachOpList, true);
    if (opList.parentNode) opList.parentNode.removeChild(opList);
    area.destroy();
  };

  return hook;
}

// ruleRpc POSTs a Twirp JSON call through the shell's bearer wrapper (window
// .apiFetch) and returns the decoded response. A non-2xx carries a Twirp error
// body ({code, msg, meta:{code}}) which is normalised into an Error with .code
// (the canonical APERTURE_* code), .message, and .status. Named ruleRpc to avoid
// colliding with the other screens' classic-script globals.
async function ruleRpc(method, body) {
  const res = await window.apiFetch(
    "/twirp/aperture.ApertureService/" + method,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    }
  );
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (_) {
      data = null;
    }
  }
  if (!res.ok) {
    const code =
      (data && data.meta && data.meta.code) ||
      (data && data.code) ||
      "APERTURE_ERROR";
    const msg = (data && (data.msg || data.message)) || res.statusText || "Request failed";
    const err = new Error(msg);
    err.code = code;
    err.status = res.status;
    throw err;
  }
  return data || {};
}

/*
 * ---- What-if preview: rendering the metadata snapshot ----------------------
 *
 * The preview shows the metadata the rule ACTUALLY saw, straight off the
 * provider. It stays strictly read-only: nothing below is an input, and there is
 * no path by which the client supplies metadata — the snapshot only ever arrives
 * on the EvaluateRule response.
 *
 * The value shape is closed, so these helpers render against a known model
 * rather than defending against arbitrary JSON. A field value is:
 *
 *   - a SCALAR (null, boolean, string, number), or
 *   - an ARRAY of scalars — an empty list cell produces a real `[]`, or
 *   - an OBJECT whose members are scalars, scalar arrays, or ONE further object
 *     level (the depth cap is 2).
 *
 * Arrays of objects are impossible: the provider rejects them at load, so an
 * array element is never a container.
 *
 * A field that was ABSENT from the object simply has no row. That is a real
 * semantic difference from an empty list or an empty object, which DO get a row
 * (`[]` / `{}` plus an "empty list" / "empty object" note), because an author
 * debugging an `in` comparison has to tell "the field is missing" apart from
 * "the field is there and holds nothing".
 */

// formatMetadataScalar renders one scalar the way the previous single-line
// JSON.stringify preview did — strings keep their quotes, everything else shows
// its JSON form. The quotes are not decoration: they distinguish the string
// "42" from the number 42, which is exactly the mismatch the preview exists to
// expose.
function formatMetadataScalar(value) {
  if (value === undefined) return "undefined"; // unreachable over JSON; total anyway
  const text = JSON.stringify(value);
  return text === undefined ? String(value) : text;
}

// metadataRows flattens a metadata snapshot into the ordered, indented row list
// the preview panel renders. Flattening (rather than nesting templates) keeps
// the markup down to a single x-for, which the depth cap of 2 makes sufficient.
//
// Field order is whatever the server sent; Go marshals a map with its keys
// sorted, so the listing is already alphabetical and stable between runs.
function metadataRows(metadata) {
  const rows = [];
  if (!metadata || typeof metadata !== "object" || Array.isArray(metadata)) {
    return rows;
  }
  Object.keys(metadata).forEach(function (name) {
    pushMetadataRows(rows, name, name, metadata[name], 0);
  });
  return rows;
}

// pushMetadataRows appends the row(s) for one value. A scalar or an array is a
// single row; an object is a header row followed by one row per member, indented
// one level. `path` is the dotted path from the field root and is used only as
// the x-for key, so it must stay unique.
function pushMetadataRows(rows, path, label, value, depth) {
  if (Array.isArray(value)) {
    // Elements are always scalars, so each renders as its own chip and a long
    // array wraps inside the panel instead of stretching it.
    rows.push({
      key: path,
      depth: depth,
      label: label,
      kind: "array",
      text: value.length === 0 ? "[]" : "",
      items: value.map(formatMetadataScalar),
      note: value.length === 0 ? "empty list" : countNote(value.length, "item"),
    });
    return;
  }
  if (value !== null && typeof value === "object") {
    const keys = Object.keys(value);
    rows.push({
      key: path,
      depth: depth,
      label: label,
      kind: "object",
      text: keys.length === 0 ? "{}" : "{…}",
      items: [],
      note: keys.length === 0 ? "empty object" : countNote(keys.length, "field"),
    });
    keys.forEach(function (k) {
      pushMetadataRows(rows, path + "." + k, k, value[k], depth + 1);
    });
    return;
  }
  rows.push({
    key: path,
    depth: depth,
    label: label,
    kind: "scalar",
    text: formatMetadataScalar(value),
    items: [],
    note: "",
  });
}

// countNote renders "1 item" / "3 items" — a plain count, no emoji, sentence
// case, per the shell's copy rules.
function countNote(n, noun) {
  return n + " " + noun + (n === 1 ? "" : "s");
}

// buildPalette assembles the node palette the shell renders (index.html walks
// `palette` -> `grp.items` and calls add(item.kind)). The Logic / Operands /
// Pulse groups are fixed — one entry per AST node type — but the Compare group
// is GENERATED from the serializer's OP_SPECS, one clickable entry per operator,
// so every operator is authorable without typing one and the group can never
// fall behind the AST.
//
// `kind` doubles as the template's x-for key, so it must stay unique; a
// comparison entry therefore carries a COMPOUND kind ("compare:hasAll") that
// rules().add() splits back into a node kind plus a seed operator. That keeps
// the operator out of the shared template and off the editor hook's signature.
function buildPalette() {
  const comparisons = operatorEntries().map(function (entry) {
    return { kind: "compare:" + entry.op, label: entry.label };
  });
  return [
    { group: "Logic", items: [
      { kind: "and", label: "And" },
      { kind: "or", label: "Or" },
      { kind: "not", label: "Not" },
    ] },
    // The plain Compare entry is the fallback for a shell whose serializer did
    // not load: without OP_SPECS there are no operator entries, and the palette
    // must still be able to place a comparison node.
    { group: "Compare", items: comparisons.length ? comparisons : [{ kind: "compare", label: "Compare" }] },
    { group: "Operands", items: [
      { kind: "var", label: "Variable" },
      { kind: "literal", label: "Literal" },
    ] },
    { group: "Pulse", items: [
      { kind: "list", label: "List" },
      { kind: "call", label: "Call" },
    ] },
  ];
}

// A small starter rule so the canvas is not blank on first open; also exercises
// fromAST end to end. object.classification == "public".
const STARTER_AST = {
  type: "compare",
  op: "eq",
  left: { type: "var", name: "object.classification" },
  right: { type: "literal", value: "public" },
};

function rules() {
  return {
    booting: true,
    error: "",
    problems: [],
    // ---- E7-S3 load/save/validate/what-if integration state ----
    // The dev principal the RPCs resolve against, plus the admin tier probe (rule
    // editing is SYSTEM tier — only system-admins may save). Rules are global, so
    // there is no account context.
    principal: "",
    accounts: [],
    canEdit: false,
    tierChecked: false,
    // The rule being edited: its name (identity, upsert key), description, and the
    // list of stored rules to load from.
    ruleName: "",
    description: "",
    ruleList: [],
    // Save / validate status and the server-side (engine) validation problems,
    // distinct from the client-side structural `problems` above.
    saving: false,
    validating: false,
    status: null, // { kind: "ok" | "err", code, msg }
    // Object-based what-if preview (READ-ONLY): pick an object type + a sample
    // object, evaluate the rule CURRENTLY on the canvas against that object's
    // provider metadata, and show the boolean result — no account/principal/grant.
    objectTypes: [],
    preview: { objectType: "", objectId: "", objects: [] },
    previewing: false,
    previewError: null,
    previewResult: null, // true | false | null (not yet run)
    previewObject: null, // the object metadata snapshot the rule saw
    // previewRows is that snapshot flattened for display by metadataRows():
    // one row per field, plus one indented row per member of an object-valued
    // field. Derived on evaluate rather than on every render so the panel does
    // not re-walk the snapshot on unrelated Alpine updates.
    previewRows: [],
    // The node palette, grouped by category. Covers the whole AST: logical
    // combinators, comparisons over variables/literals, and the Pulse building
    // blocks (list/call) from E2-S3. The Compare group is generated from the
    // operator registry — see buildPalette().
    palette: buildPalette(),
    _editor: null,

    init() {
      this.principal = localStorage.getItem("aperture.devToken") || "";
      document.addEventListener("aperture:authenticated", (e) => {
        this.principal = (e.detail && e.detail.principal) || localStorage.getItem("aperture.devToken") || "";
        this.bootstrap();
      });
      const clear = () => {
        this.principal = "";
        this.accounts = [];
        this.canEdit = false;
        this.tierChecked = false;
      };
      document.addEventListener("aperture:unauthenticated", clear);
      document.addEventListener("aperture:signout", clear);
      this.mount();
    },

    async mount() {
      this.booting = true;
      this.error = "";
      try {
        if (!window.RuleSerializer) {
          throw new Error("rule serializer not loaded");
        }
        const mod = await import("/vendor/rete/rete.min.js");
        const el = this.$refs.canvas;
        if (!el) {
          throw new Error("canvas container not found");
        }
        this._editor = await createBlueprintEditor(el, mod, window.RuleSerializer);
        // Expose the E7-S3 save/load hooks and wire validation refresh.
        window.blueprintEditor = this._editor;
        this._editor.onChange = () => {
          this.refreshValidation();
          // A graph edit invalidates the last server verdict and preview.
          this.status = null;
        };
        await this._editor.fromAST(STARTER_AST);
        this.booting = false;
        if (this.principal) this.bootstrap();
      } catch (e) {
        this.error = e && e.message ? e.message : String(e);
        this.booting = false;
      }
    },

    // bootstrap probes system-admin authority (rule editing is SYSTEM tier) and
    // lists the stored rules to load from. Rules are global — nothing here depends
    // on an account.
    async bootstrap() {
      await this.probeTier();
      await this.loadRules();
      await this.loadObjectTypes();
    },

    // loadObjectTypes fills the preview's object-type picker. A rule is not tied
    // to a type (a scope strategy references it by name), so the author chooses
    // which type to sample an object from.
    async loadObjectTypes() {
      try {
        const resp = await ruleRpc("ListObjectTypes", {});
        this.objectTypes = (resp.entities_json || [])
          .map((s) => JSON.parse(s).Name)
          .filter(Boolean);
      } catch (e) {
        if (e.status !== 401) this.status = { kind: "err", code: e.code, msg: e.message };
      }
    },

    // loadPreviewObjects samples the chosen object type's instance ids from its
    // provider into the object picker. A type with no provider yields
    // APERTURE_PROVIDER_UNREGISTERED, surfaced as a preview error.
    async loadPreviewObjects() {
      this.preview.objectId = "";
      this.preview.objects = [];
      this.previewResult = null;
      this.previewObject = null;
      this.previewRows = [];
      this.previewError = null;
      if (!this.preview.objectType) return;
      try {
        const resp = await ruleRpc("ObjectIdentifiers", { object_type: this.preview.objectType });
        this.preview.objects = resp.object_ids || [];
        if (this.preview.objects.length === 0) {
          this.previewError = {
            code: "APERTURE_NOT_FOUND",
            msg: 'No objects available for type "' + this.preview.objectType + '".',
          };
        }
      } catch (e) {
        this.previewError = { code: e.code || "APERTURE_ERROR", msg: e.message };
      }
    },


    // probeTier asks the OPEN Check RPC whether the signed-in principal holds
    // system-admin authority, gating the Save affordance (E6-S2 pattern). The
    // check is global and account-independent — it resolves system-admin via the
    // platform "*" grant. It carries the bearer but needs no auth, so a read-only
    // viewer still gets an answer and a 403 on save is never a surprise.
    async probeTier() {
      this.tierChecked = false;
      try {
        const dec = await ruleRpc("Check", {
          account: "*", // resolve the system-admin grant in the platform "*" account
          principal: this.principal,
          action: "aperture.admin",
          object: "system:schema",
        });
        this.canEdit = !!dec.allow;
      } catch (_) {
        this.canEdit = false;
      }
      this.tierChecked = true;
    },

    // loadRules lists the stored rules for the load picker.
    async loadRules() {
      try {
        const resp = await ruleRpc("ListRules", {});
        this.ruleList = (resp.rules_json || []).map((s) => JSON.parse(s));
      } catch (e) {
        if (e.status !== 401) this.status = { kind: "err", code: e.code, msg: e.message };
      }
    },

    // loadRule fetches one rule by name and renders its AST into the canvas via the
    // fromAST hook — the same rules.Node JSON the engine evaluates and the state
    // file persists.
    async loadRule(name) {
      if (!name) return;
      this.status = null;
      try {
        const resp = await ruleRpc("GetRule", { id: name });
        const rule = JSON.parse(resp.rule_json);
        this.ruleName = rule.Name || name;
        this.description = rule.Description || "";
        if (rule.AST && window.blueprintEditor) {
          await window.blueprintEditor.fromAST(rule.AST);
        }
        this.refreshValidation();
      } catch (e) {
        if (e.status !== 401) this.status = { kind: "err", code: e.code, msg: e.message };
      }
    },

    // currentRule folds the canvas to a model.Rule body ({Name, Description, AST}).
    // toAST throws a structured {code,message} when the graph is not a single tree;
    // the caller surfaces it as a status error.
    currentRule() {
      if (!window.blueprintEditor) throw new Error("editor not ready");
      const ast = window.blueprintEditor.toAST();
      if (ast === null) {
        const err = new Error("the canvas is empty — build a rule before saving");
        err.code = "APERTURE_RULE_INVALID";
        throw err;
      }
      return { Name: this.ruleName.trim(), Description: this.description, AST: ast };
    },

    // serverValidate compiles/validates the current AST on the server WITHOUT
    // persisting it, surfacing the engine's APERTURE_RULE_* verdict on the canvas.
    async serverValidate() {
      this.status = null;
      this.validating = true;
      try {
        const rule = this.currentRule();
        await ruleRpc("ValidateRule", { rule_json: JSON.stringify(rule) });
        this.status = { kind: "ok", msg: "The rule compiled cleanly on the server." };
      } catch (e) {
        this.status = { kind: "err", code: e.code || "APERTURE_RULE_INVALID", msg: e.message };
      } finally {
        this.validating = false;
      }
    },

    // save persists the current rule via the mutation API (system-admin tier). The
    // server re-validates the AST and rejects an invalid rule with its
    // APERTURE_RULE_* code, shown on the canvas; on success the rule takes effect
    // immediately in any scope strategy that references it.
    async save() {
      this.status = null;
      if (!this.ruleName.trim()) {
        this.status = { kind: "err", code: "APERTURE_RULE_INVALID", msg: "A rule name is required." };
        return;
      }
      this.saving = true;
      try {
        const rule = this.currentRule();
        await ruleRpc("PutRule", {
          // Rules are system-tier: the actor resolves system-admin in the "*"
          // account, so it must carry it (an empty actor account is rejected).
          actor: { principal: this.principal, account: "*" },
          rule_json: JSON.stringify(rule),
        });
        this.status = { kind: "ok", msg: 'Saved rule "' + rule.Name + '".' };
        await this.loadRules();
      } catch (e) {
        this.status = { kind: "err", code: e.code || "APERTURE_ERROR", msg: e.message };
      } finally {
        this.saving = false;
      }
    },

    // runPreview evaluates the rule CURRENTLY on the canvas against the selected
    // object's provider metadata and shows the boolean result, WITHOUT saving. No
    // account/principal/grant is involved — the rule reads only object.*, so the
    // author sees it fire (or not) directly. The rule need not even be named yet.
    async runPreview() {
      this.previewError = null;
      this.previewResult = null;
      this.previewObject = null;
      this.previewRows = [];
      if (!this.preview.objectId) {
        this.previewError = { code: "APERTURE_INVALID_INPUT", msg: "Select an object to evaluate the rule against." };
        return;
      }
      this.previewing = true;
      try {
        const rule = this.currentRule();
        const resp = await ruleRpc("EvaluateRule", {
          rule_json: JSON.stringify(rule),
          object_id: this.preview.objectId,
        });
        this.previewResult = !!resp.result;
        this.previewObject = resp.object_json ? JSON.parse(resp.object_json) : null;
        this.previewRows = metadataRows(this.previewObject);
      } catch (e) {
        this.previewError = { code: e.code || "APERTURE_ERROR", msg: e.message };
      } finally {
        this.previewing = false;
      }
    },

    // refreshValidation runs the client-side structural check and surfaces its
    // problems. Full type-checking is the engine's (server-side, E7-S3).
    refreshValidation() {
      if (!window.blueprintEditor) return;
      try {
        this.problems = window.blueprintEditor.validate();
        this.error = "";
      } catch (e) {
        this.problems = [];
        this.error = e && e.message ? e.message : String(e);
      }
    },

    // add places a palette entry on the canvas. A comparison entry carries its
    // operator in a compound kind ("compare:hasAll") — see buildPalette() — so
    // it is split here into the node kind and the seed operator the new node
    // opens with.
    add(kind) {
      if (!window.blueprintEditor) return;
      const sep = String(kind).indexOf(":");
      if (sep < 0) {
        window.blueprintEditor.addNode(kind);
        return;
      }
      window.blueprintEditor.addNode(String(kind).slice(0, sep), {
        op: String(kind).slice(sep + 1),
      });
    },

    async clearCanvas() {
      if (window.blueprintEditor) {
        await window.blueprintEditor.clear();
        this.refreshValidation();
      }
    },

    zoomToFit() {
      if (window.blueprintEditor) {
        window.blueprintEditor.zoomToFit();
      }
    },

    get valid() {
      return this.problems.length === 0 && !this.error;
    },

    destroy() {
      if (this._editor && typeof this._editor.destroy === "function") {
        this._editor.destroy();
      }
      if (window.blueprintEditor === this._editor) {
        window.blueprintEditor = null;
      }
      this._editor = null;
    },
  };
}

window.rules = rules;
window.createBlueprintEditor = createBlueprintEditor;
// Exported so the preview's snapshot rendering can be exercised directly from a
// console against a real EvaluateRule payload; the panel itself reads
// previewRows off the Alpine component.
window.metadataRows = metadataRows;
