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
  // The eight date operators read as sentences rather than as tokens: an author
  // scanning the palette for "is on or after" should not have to recognise
  // `onOrAfter`, and the three same-* operators are calendar-BUCKET equality
  // ("is in the same year as"), not distance, which the bare token does not say.
  before: "is before",
  after: "is after",
  onOrBefore: "is on or before",
  onOrAfter: "is on or after",
  between: "is between",
  sameDay: "is on the same day as",
  sameMonth: "is in the same month as",
  sameYear: "is in the same year as",
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

// opListSeq numbers each mounted editor so two of them (the shell re-creates the
// section on navigation) never share a <datalist> element id. It counts EDITORS,
// not lists: every list an editor owns is suffixed with the same number, so one
// counter still guarantees uniqueness however many vocabularies gain a dropdown.
let opListSeq = 0;

/*
 * ---- The relative-date vocabularies ---------------------------------------
 *
 * A relativeDate node carries four fields — anchor, n, unit, snap — and three of
 * them are drawn from CLOSED sets the serializer mirrors out of `rules/relative.go`
 * (ANCHORS / UNITS / SNAPS, compared entry-for-entry by the Go contract test).
 * The editor NEVER hand-lists a member: it reads the tables, in the presentation
 * order the serializer chose, so a unit added on the Go side appears in the
 * dropdown with no edit here and a unit the editor invented could not exist.
 *
 * NEVER build a JS Date out of any of this, and never render a stored date
 * through toLocaleString(). The engine is UTC end to end and the editor shows
 * date strings verbatim, `Z` included: a viewer in UTC-5 would read a stored
 * 2026-01-01T00:00:00Z as "2025-12-31 19:00" — a different calendar YEAR. That
 * is a correctness rule, not a preference.
 */

// vocab reads one of the serializer's string tables, tolerating a serializer
// that has not loaded exactly as opSpecs() does.
function vocab(name) {
  const S = typeof window !== "undefined" ? window.RuleSerializer : null;
  const list = S ? S[name] : null;
  return Array.isArray(list) ? list : [];
}

// vocabLabel renders a vocabulary token as its readable spelling. It is DERIVED,
// never a second table: an ALL-CAPS token lowercases (`NOW` -> "now") and a
// camelCase one splits into words (`startOfYear` -> "start of year"). A table
// would be a third hand-maintained copy of a set that is already written down
// twice, and would silently show a bare token for any member it had not caught
// up with — deriving cannot fall behind.
function vocabLabel(token) {
  const s = String(token === undefined || token === null ? "" : token);
  if (s === "") return "";
  if (/^[A-Z0-9_]+$/.test(s)) return s.toLowerCase().replace(/_/g, " ");
  return s.replace(/([A-Z])/g, function (m) {
    return " " + m.toLowerCase();
  });
}

// vocabFromLabel is the inverse against a closed list, mirroring opFromLabel: an
// exact member wins, then a case- and whitespace-insensitive spelling match, and
// anything unrecognised is returned VERBATIM.
//
// Returning it verbatim is the point. A <datalist> is a suggestion list, not a
// constraint — the browser lets an author type anything into the field — so a
// typed `fortnights` must reach validateAST as `fortnights` and be reported as
// "relative date has an unknown offset unit: fortnights", which is the Go
// validator's own wording. Substituting something plausible here would save a
// rule the author never wrote.
function vocabFromLabel(list, text) {
  const raw = String(text === undefined || text === null ? "" : text).trim();
  if (list.indexOf(raw) >= 0) return raw;
  const norm = raw.toLowerCase().replace(/\s+/g, " ");
  for (let i = 0; i < list.length; i++) {
    if (vocabLabel(list[i]).toLowerCase() === norm) return list[i];
  }
  return raw;
}

// vocabOptions renders a vocabulary as the readable option list a <datalist>
// shows, preserving the serializer's presentation order (anchors as declared,
// units coarsest-first, snaps with the identity `none` leading).
function vocabOptions(name) {
  return vocab(name).map(vocabLabel);
}

// defaultMember picks the value a fresh relative-date control opens with. The
// preference is a readable starting point (`NOW`, `days`, `none`) but membership
// still comes from the table: if the preferred member is not in it, the table's
// first entry is used, so this can never seed a node with a token the server
// does not know.
function defaultMember(name, preferred) {
  const list = vocab(name);
  if (list.indexOf(preferred) >= 0) return preferred;
  return list.length > 0 ? list[0] : "";
}

// offsetInitial seeds the offset control. A number passes through so a loaded
// rule round-trips byte-for-byte; an absent/blank offset opens at 0 ("no
// offset", which is what the four controls mean before an author touches them);
// and anything else — a malformed `n` in a rule loaded from the server — is kept
// VERBATIM so the validator names it instead of the control hiding it.
function offsetInitial(value) {
  if (typeof value === "number") return value;
  if (value === null || value === undefined) return 0;
  return String(value).trim() === "" ? 0 : value;
}

// isDateOp / isBoundsOp classify an operator for the operand affordances below.
// Both defer to the serializer — date-ness is the DATE_OPS table it mirrors out
// of Go, and the ternary shape is the operator's right-operand shape — so
// neither answer is re-derived from a list kept here.
function isDateOp(op) {
  const S = typeof window !== "undefined" ? window.RuleSerializer : null;
  return !!(S && typeof S.isDateOp === "function" && S.isDateOp(op));
}

function isBoundsOp(op) {
  const S = typeof window !== "undefined" ? window.RuleSerializer : null;
  const spec = opSpecs()[op];
  return !!(S && S.RIGHT && spec && spec.right === S.RIGHT.BOUNDS);
}

/*
 * ---- The right-operand mode switch ----------------------------------------
 *
 * A date operator's right operand can be three different node types, and which
 * one an author wants is a decision they make BEFORE they have a node to wire:
 * a literal date typed by hand, a relative date built from the four controls, or
 * another date field on the object. Making them find the right palette entry and
 * drag a wire for that is the friction this story exists to remove.
 *
 * So a date comparison carries a MODE control per operand slot (one for the
 * single-operand operators, two for `between`'s independent bounds). Choosing a
 * mode builds the operand node and wires it in; the mode itself is NOT persisted
 * anywhere — it is read back off the graph, so a rule loaded from the server, or
 * an operand rewired by hand, shows the mode that matches what is actually
 * there. There is no editor state that can disagree with the AST.
 */

// MODES are the operand kinds a mode control offers, in the order the dropdown
// lists them: the two the story is about first, then the variable case (a date
// field compared against another date field), which the same machinery gets for
// free and which the mode read-back has to be able to name anyway.
const MODES = ["literal", "relative", "variable"];

// MODE_CONTROL_KEYS is every control key the mode switch may occupy on a
// comparison node. `mode` is the single right operand; `lower` / `upper` are
// `between`'s two bounds, which are INDEPENDENT — all four literal/relative
// combinations are authorable because neither control consults the other.
const MODE_CONTROL_KEYS = ["mode", "lower", "upper"];

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
      // The operand-mode controls are DERIVED from the graph, so they are
      // re-read here — on the one debounced hook every structural edit already
      // funnels through — rather than being kept in step by each mutation site.
      reconcileModeControls();
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
      // A date operator additionally carries its operand mode switch(es). They
      // are added here rather than after the node is mounted so a comparison
      // dropped from the palette already shows them, and re-synced whenever the
      // operator changes (applyCompareInputs).
      syncModeControls(node, node.op);
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
    if (kind === S.TYPES.RELATIVE_DATE) {
      // Four flat controls, in the order the node reads as a sentence: "the
      // start of the year, five years back" — anchor, offset, unit, snap. The
      // server applies the SNAP FIRST and then the offset, so the offset always
      // means "n units from the named boundary".
      //
      // All four are always present on the node; there is no "unset" state.
      // "No offset" is 0 (with whatever unit) and "no snap" is the vocabulary
      // member `none`, so an empty control is a validation problem rather than a
      // silently different rule.
      //
      // The three closed-set controls are text inputs bound to a <datalist>
      // (attached by delegation below) because Rete's classic control set is
      // text/number only and the React renderer is a committed vendored blob
      // this repo never rebuilds — a real <select> would mean a node build.
      node.addControl(
        "anchor",
        new ClassicPreset.InputControl("text", {
          initial: vocabLabel(d.anchor === undefined ? defaultMember("ANCHORS", "NOW") : d.anchor),
          change: fireChange,
        })
      );
      // The offset's sign IS the direction: negative goes into the past. There
      // is deliberately no ago/from-now toggle — a second field meaning the same
      // thing as the sign is a second way to write the same rule, and the AST
      // has one.
      //
      // It is a TEXT control, not a number one, and that is load bearing for
      // exactly that reason. The classic number control coerces its input with
      // `+value` on every keystroke, and a number input whose content is the
      // single character "-" reports an EMPTY value — so `+""` is 0, the control
      // re-renders as "0", and the minus the author just typed is wiped before
      // they can type a digit. Typing -3 yields 03. A control that cannot express
      // "into the past" cannot express this node.
      //
      // A text control also matches what the serializer promises: normalizeOffset
      // turns a blank into 0 and keeps an unparseable token VERBATIM so
      // validateAST can report "relative date offset must be a whole number: 1.5"
      // in the Go validator's own words. A number control could never produce
      // that token, so the fraction would be silently coerced instead of named.
      node.addControl(
        "n",
        new ClassicPreset.InputControl("text", {
          initial: String(offsetInitial(d.n)),
          change: fireChange,
        })
      );
      node.addControl(
        "unit",
        new ClassicPreset.InputControl("text", {
          initial: vocabLabel(d.unit === undefined ? defaultMember("UNITS", "days") : d.unit),
          change: fireChange,
        })
      );
      node.addControl(
        "snap",
        new ClassicPreset.InputControl("text", {
          initial: vocabLabel(d.snap === undefined ? defaultMember("SNAPS", "none") : d.snap),
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
    const prev = node.op;
    node.op = opFromLabel(text);
    syncCompareInputs(node, node.op, prev);
  }

  // syncCompareInputs serialises the re-shape per node. The control fires on
  // every keystroke, and applyCompareInputs awaits (connection removal, area
  // update), so two edits in flight could otherwise interleave and add the same
  // pin twice. Chaining on the node keeps them strictly ordered; a rejected link
  // must not stall the chain, hence the same handler for both outcomes.
  function syncCompareInputs(node, op, prevOp) {
    const apply = function () {
      return applyCompareInputs(node, op, prevOp);
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
  async function applyCompareInputs(node, op, prevOp) {
    const want = S.inputKeys(node.kind, node.arity || 0, op);
    const have = Object.keys(node.inputs || {});
    const drop = have.filter(function (k) {
      return want.indexOf(k) < 0;
    });
    const add = want.filter(function (k) {
      return have.indexOf(k) < 0;
    });

    // The operand-shape work runs only for a KNOWN operator. The control fires
    // on every keystroke, so an author retyping `is before` over `is between`
    // passes through a run of unrecognised half-words — and reacting to those
    // would tear down the bounds and delete a configured relative date on the
    // way to an operator the author is still spelling. An unknown operator is
    // reported by the validator; it is never a reason to touch the graph.
    let reshaped = false;
    if (opSpecs()[op]) {
      reshaped = await reshapeDateOperands(node, prevOp, op);
      if (syncModeControls(node, op)) reshaped = true;
    }

    if (drop.length === 0 && add.length === 0) {
      if (reshaped) await area.update("node", node.id);
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
   * ---- Date operands: the mode switch and `between`'s bounds ---------------
   *
   * Everything below turns "which kind of thing is on the right of this date
   * comparison?" into one control, and keeps `between`'s ternary shape — a
   * two-item `list` on the right — something the editor builds rather than
   * something an author has to know about and hand-wire.
   *
   * The rules it follows, in one place because they are easy to get subtly
   * wrong:
   *
   *   - The mode is DERIVED from the graph, never stored. Nothing here is part
   *     of the AST, so a rule loaded from the server and a rule built by hand
   *     show the same modes for the same shape.
   *   - Compatible operands SURVIVE an operator change: `is before` -> `is
   *     after` touches nothing at all, because both take one date operand.
   *   - An INCOMPATIBLE operand is cleared: a relative date is legal only under
   *     a date operator, so moving to `has all` removes it rather than leaving a
   *     node that saves as an error the canvas does not explain.
   *   - Entering `between` WRAPS the current operand as the lower bound;
   *     leaving it UNWRAPS the lower bound back onto `right`. The author's
   *     configured operand survives the round trip in the position that still
   *     means the same thing.
   *   - Only LEAVES are ever deleted (a var, a literal, a relative date — one
   *     field, retypeable). A displaced subtree is disconnected and left on the
   *     canvas: the editor does not throw away structure someone built. An
   *     orphan shows up as a second root in validation until it is rewired or
   *     removed, which is the existing behaviour for any unwired operand.
   */

  // MODE_KIND maps a mode control's value to the node type it builds, and
  // MODE_OF_KIND is the read-back. A node type no mode names (a call, a list,
  // another comparison) reads back as "" — a blank control, never a wrong one.
  const MODE_KIND = {
    literal: S.TYPES.LITERAL,
    relative: S.TYPES.RELATIVE_DATE,
    variable: S.TYPES.VAR,
  };
  const MODE_OF_KIND = {};
  Object.keys(MODE_KIND).forEach(function (mode) {
    MODE_OF_KIND[MODE_KIND[mode]] = mode;
  });

  // wiredSource returns the node feeding `target`'s `key` input, or null.
  function wiredSource(target, key) {
    if (!target) return null;
    const conns = editor.getConnections();
    for (let i = 0; i < conns.length; i++) {
      if (conns[i].target === target.id && conns[i].targetInput === key) {
        return editor.getNode(conns[i].source) || null;
      }
    }
    return null;
  }

  // isLeafOperand reports whether a node is a single-field operand — derived
  // from the serializer's NODE_SPECS (`inputs: "none"`), so a leaf added to the
  // AST later is classified without an edit here.
  function isLeafOperand(node) {
    const spec = node ? S.NODE_SPECS[node.kind] : null;
    return !!spec && spec.inputs === "none";
  }

  // unwire removes the connection into `target[key]` and returns the node that
  // fed it, WITHOUT removing that node.
  async function unwire(target, key) {
    const src = wiredSource(target, key);
    const stale = editor.getConnections().filter(function (c) {
      return c.target === target.id && c.targetInput === key;
    });
    for (const c of stale) {
      await editor.removeConnection(c.id);
    }
    return src;
  }

  // clearOperand unwires `target[key]` and removes the displaced node when it is
  // a leaf (see the deletion rule above).
  async function clearOperand(target, key) {
    const src = await unwire(target, key);
    if (src && isLeafOperand(src)) {
      await editor.removeNode(src.id);
    }
  }

  // viewPosition reads a node's canvas position, defaulting to the origin when
  // the view is not mounted yet (a node added in the same tick).
  function viewPosition(node) {
    try {
      const view = area.nodeViews.get(node.id);
      if (view && view.position) return view.position;
    } catch (_) {
      /* positioning is cosmetic; never break an edit over it */
    }
    return { x: 0, y: 0 };
  }

  // placeNear drops a freshly built operand to the LEFT of the node it feeds,
  // which is the direction the graph already flows, so it lands where the author
  // is looking instead of on top of whatever sits at the origin. `row` staggers
  // `between`'s two bounds so they do not overlap.
  async function placeNear(node, target, row) {
    const at = viewPosition(target);
    await area.translate(node.id, { x: at.x - 260, y: at.y + (row || 0) * 96 });
  }

  // setOperand replaces whatever is wired into `target[key]` with a fresh node of
  // `kind`, seeded from `data`, and returns it.
  async function setOperand(target, key, kind, data, row) {
    await clearOperand(target, key);
    const node = makeNode(kind, data || {});
    await editor.addNode(node);
    await placeNear(node, target, row);
    await editor.addConnection(
      new ClassicPreset.Connection(node, "out", target, key)
    );
    return node;
  }

  // boundsList returns the two-item `list` node carrying a `between`'s bounds,
  // or null when the right operand is not one (nothing wired yet, or an author
  // wired something else — which validation reports rather than this rewriting).
  function boundsList(node) {
    const right = wiredSource(node, "right");
    return right && right.kind === S.TYPES.LIST ? right : null;
  }

  // wrapBounds gives a comparison the ternary shape `between` requires: a
  // two-item list on the right. The operand already on the right (if any) keeps
  // its meaning as the LOWER bound; the other bound is scaffolded as an empty
  // literal so both are immediately editable and both mode switches have
  // something to read.
  async function wrapBounds(node) {
    if (boundsList(node)) return false;
    const existing = await unwire(node, "right");
    const list = makeNode(S.TYPES.LIST, { arity: 2 });
    await editor.addNode(list);
    await placeNear(list, node, 0);
    await editor.addConnection(
      new ClassicPreset.Connection(list, "out", node, "right")
    );
    if (existing) {
      await editor.addConnection(
        new ClassicPreset.Connection(existing, "out", list, "in-0")
      );
      // The operand was sitting where the list now is, so it is moved one column
      // further left. Cosmetic, but a bound hidden under the node that wraps it
      // reads as if the editor lost it.
      await placeNear(existing, list, 0);
    } else {
      await setOperand(list, "in-0", S.TYPES.LITERAL, { value: "" }, 0);
    }
    await setOperand(list, "in-1", S.TYPES.LITERAL, { value: "" }, 1);
    return true;
  }

  // unwrapBounds is the inverse: the lower bound becomes the single right
  // operand and the list goes. The upper bound has nowhere left to sit — the
  // operator now takes one value — so it follows the deletion rule: a leaf is
  // removed with the list, anything larger is left on the canvas.
  async function unwrapBounds(node) {
    const list = boundsList(node);
    if (!list) return false;
    const lower = await unwire(list, "in-0");
    const upper = await unwire(list, "in-1");
    await unwire(node, "right");
    await editor.removeNode(list.id);
    if (lower) {
      await editor.addConnection(
        new ClassicPreset.Connection(lower, "out", node, "right")
      );
    }
    if (upper && isLeafOperand(upper)) {
      await editor.removeNode(upper.id);
    }
    return true;
  }

  // clearRelativeOperands drops any relative date sitting under a comparison
  // that is no longer a date comparison, wherever it sits (the single right
  // operand, or either bound of a list still wired there).
  async function clearRelativeOperands(node) {
    let changed = false;
    const right = wiredSource(node, "right");
    if (right && right.kind === S.TYPES.RELATIVE_DATE) {
      await clearOperand(node, "right");
      return true;
    }
    if (right && right.kind === S.TYPES.LIST) {
      const keys = Object.keys(right.inputs || {});
      for (const key of keys) {
        const bound = wiredSource(right, key);
        if (bound && bound.kind === S.TYPES.RELATIVE_DATE) {
          await clearOperand(right, key);
          changed = true;
        }
      }
    }
    return changed;
  }

  // reshapeDateOperands adapts a comparison's right operand to an operator
  // change. It returns whether the graph changed, so the caller knows to
  // re-render. See the block comment above for the rules it applies.
  async function reshapeDateOperands(node, prevOp, op) {
    if (node.kind !== S.TYPES.COMPARE) return false;
    const wasBounds = isBoundsOp(prevOp);
    const nowBounds = isBoundsOp(op);
    let changed = false;
    if (nowBounds && !wasBounds) {
      changed = (await wrapBounds(node)) || changed;
    } else if (wasBounds && !nowBounds) {
      changed = (await unwrapBounds(node)) || changed;
    }
    if (!isDateOp(op)) {
      changed = (await clearRelativeOperands(node)) || changed;
    }
    return changed;
  }

  // modeSlots describes where each of a comparison's mode controls reads and
  // writes: the control key, the node holding the operand pin, and that pin's
  // key. `target` is null for a `between` whose bounds list is missing, which is
  // the one case a mode change has to build before it can act.
  function modeSlots(node) {
    if (node.kind !== S.TYPES.COMPARE || !isDateOp(node.op)) return [];
    if (!isBoundsOp(node.op)) {
      return [{ control: "mode", target: node, key: "right" }];
    }
    const list = boundsList(node);
    return [
      { control: "lower", target: list, key: "in-0" },
      { control: "upper", target: list, key: "in-1" },
    ];
  }

  // syncModeControls makes the node's mode controls equal what `op` calls for —
  // one for the single-operand date operators, two for `between`, none for
  // anything else. Like the pin diff it is a diff, so switching among the date
  // operators that share a shape leaves the control (and its value) alone.
  function syncModeControls(node, op) {
    const want = isDateOp(op)
      ? isBoundsOp(op)
        ? ["lower", "upper"]
        : ["mode"]
      : [];
    let changed = false;
    MODE_CONTROL_KEYS.forEach(function (key) {
      const has = !!(node.controls && node.controls[key]);
      if (want.indexOf(key) >= 0 && !has) {
        node.addControl(
          key,
          new ClassicPreset.InputControl("text", {
            initial: "",
            change: function (v) {
              onModeChange(node, key, v);
            },
          })
        );
        changed = true;
      } else if (want.indexOf(key) < 0 && has) {
        node.removeControl(key);
        changed = true;
      }
    });
    return changed;
  }

  // onModeChange runs on every keystroke in a mode control. It acts only on a
  // value that RESOLVES to a known mode and actually differs from what is wired,
  // so the half-words an author types on the way to "relative" do nothing, and
  // re-picking the current mode does not rebuild (and blank) a configured
  // operand.
  function onModeChange(node, controlKey, text) {
    const mode = vocabFromLabel(MODES, text);
    if (MODES.indexOf(mode) < 0) return;
    const apply = function () {
      return applyOperandMode(node, controlKey, mode);
    };
    node.modeSync = (node.modeSync || Promise.resolve()).then(apply, apply);
    return node.modeSync;
  }

  async function applyOperandMode(node, controlKey, mode) {
    let slot = findSlot(modeSlots(node), controlKey);
    if (!slot) return;
    if (!slot.target) {
      // A `between` whose bounds list has not been built yet (an AST loaded with
      // something else on the right, or a list an author removed). Build it, then
      // re-resolve — the slot's target only exists once the list does.
      await wrapBounds(node);
      slot = findSlot(modeSlots(node), controlKey);
      if (!slot || !slot.target) return;
    }
    const current = wiredSource(slot.target, slot.key);
    if (current && MODE_OF_KIND[current.kind] === mode) return;
    const row = slot.key === "in-1" ? 1 : 0;
    if (mode === "literal") {
      await setOperand(slot.target, slot.key, S.TYPES.LITERAL, { value: "" }, row);
    } else if (mode === "variable") {
      await setOperand(slot.target, slot.key, S.TYPES.VAR, {}, row);
    } else {
      await setOperand(slot.target, slot.key, S.TYPES.RELATIVE_DATE, {}, row);
    }
    await area.update("node", node.id);
    fireChange();
  }

  function findSlot(slots, controlKey) {
    for (let i = 0; i < slots.length; i++) {
      if (slots[i].control === controlKey) return slots[i];
    }
    return null;
  }

  // reconcileModeControls re-reads every mode control off the graph. It is the
  // half that makes the mode DERIVED rather than stored: a rule rendered by
  // fromAST, an operand rewired by hand, a bound deleted — each shows up here,
  // and the control follows the graph instead of the graph following a control
  // the author never touched.
  //
  // It only ever writes control VALUES, never nodes or connections, so it cannot
  // feed itself; and it re-renders a node only when a value actually changed, so
  // it does not steal focus from an unrelated edit.
  function reconcileModeControls() {
    let nodes;
    try {
      nodes = editor.getNodes();
    } catch (_) {
      return;
    }
    nodes.forEach(function (node) {
      if (node.kind !== S.TYPES.COMPARE) return;
      let changed = false;
      modeSlots(node).forEach(function (slot) {
        const control = node.controls ? node.controls[slot.control] : null;
        if (!control) return;
        const src = slot.target ? wiredSource(slot.target, slot.key) : null;
        const want = src ? MODE_OF_KIND[src.kind] || "" : "";
        if (control.value !== want) {
          // Assigned, NOT setValue(): the classic control's setValue fires its
          // own `change` option, which here is onModeChange — reading the graph
          // would then rebuild the operand it just read, on every reconcile. The
          // renderer picks the new value up off the model on the re-render below.
          control.value = want;
          changed = true;
        }
      });
      if (changed) {
        Promise.resolve(area.update("node", node.id)).catch(function () {
          /* a re-render failure must not break the edit pipeline */
        });
      }
    });
  }

  /*
   * ---- The dropdowns -------------------------------------------------------
   *
   * Rete's classic control set is text/number only, and the React renderer that
   * draws the nodes is a COMMITTED VENDORED BLOB (vendor/rete/rete.min.js) that
   * this repo never rebuilds — so there is no custom <select> control to
   * register without a node build. Every dropdown here is therefore a text input
   * bound to a native <datalist>: the browser turns it into a picker of readable
   * spellings, typing narrows it, and the field still accepts a raw token. No
   * framework, no dependency, no vendor change.
   *
   * A <datalist> SUGGESTS; it does not constrain. An author can type anything
   * into the field, which is exactly why every one of these values is resolved
   * back through vocabFromLabel (unrecognised text passes through verbatim) and
   * checked by validateAST against the same closed set the Go validator uses.
   * The control is an affordance, never the validation.
   *
   * The lists are attached by DELEGATION rather than at node-build time because
   * the input element belongs to the renderer: it is created (and re-created) by
   * React, out of reach of the node model. Tagging it on pointerdown/focusin —
   * before the browser opens the list — is idempotent and survives any re-render.
   */

  // One sequence number per EDITOR, shared by all of its lists, so two mounted
  // editors can never collide however many dropdowns a node grows.
  const listSeq = ++opListSeq;
  const listIds = {};
  const listElements = [];

  // makeList builds one <datalist> from readable option values and returns its
  // element id.
  function makeList(name, options) {
    const id = "a-rule-" + name + "-" + listSeq;
    const list = document.createElement("datalist");
    list.id = id;
    options.forEach(function (value) {
      const option = document.createElement("option");
      option.value = value;
      list.appendChild(option);
    });
    container.appendChild(list);
    listIds[name] = id;
    listElements.push(list);
    return id;
  }

  makeList(
    "ops",
    operatorEntries().map(function (entry) {
      return entry.label;
    })
  );
  makeList("anchors", vocabOptions("ANCHORS"));
  makeList("units", vocabOptions("UNITS"));
  makeList("snaps", vocabOptions("SNAPS"));
  makeList("modes", MODES);

  // CONTROL_HINTS decorates the bare inputs the classic renderer draws, keyed by
  // "<node kind>:<control key>". Beyond the dropdown it carries the field's
  // NAME: the renderer draws no label, so without a placeholder and a title the
  // relative-date node would be four anonymous boxes. (The persistent labels are
  // CSS ::before rules on the renderer's own data-testid wrappers — see
  // css/styles.css; nothing is injected into React's DOM.)
  const CONTROL_HINTS = {};
  CONTROL_HINTS[S.TYPES.COMPARE + ":op"] = {
    list: "ops",
    title: "The comparison operator.",
  };
  const modeHint = {
    list: "modes",
    placeholder: "literal or relative",
    title:
      "What this operand is: a literal date you type, a relative date built from the four controls, or another date field on the object.",
  };
  CONTROL_HINTS[S.TYPES.COMPARE + ":mode"] = modeHint;
  CONTROL_HINTS[S.TYPES.COMPARE + ":lower"] = modeHint;
  CONTROL_HINTS[S.TYPES.COMPARE + ":upper"] = modeHint;
  CONTROL_HINTS[S.TYPES.RELATIVE_DATE + ":anchor"] = {
    list: "anchors",
    placeholder: "now",
    title: "The instant the offset is measured from. `today` is `now` at the start of its UTC day.",
  };
  CONTROL_HINTS[S.TYPES.RELATIVE_DATE + ":n"] = {
    // inputmode (not type=number — see the control) asks a touch keyboard for
    // digits and a minus without handing the field's value to browser coercion.
    inputmode: "numeric",
    placeholder: "0",
    title: "A whole number of units. Negative goes into the past; 0 means no offset.",
  };
  CONTROL_HINTS[S.TYPES.RELATIVE_DATE + ":unit"] = {
    list: "units",
    placeholder: "days",
    title: "The offset unit. Years, quarters and months are calendar units and clamp at month end.",
  };
  CONTROL_HINTS[S.TYPES.RELATIVE_DATE + ":snap"] = {
    list: "snaps",
    placeholder: "none",
    title:
      "The calendar boundary the anchor is rounded to BEFORE the offset is applied. An end-of boundary is the last second of its period.",
  };

  // controlSlot resolves an input element to the node and control key it
  // renders. The key comes from the renderer's own data-testid ("control-<key>")
  // on each control's wrapper — a stable, documented hook, not a renderer
  // internal. A bundle that stopped emitting it degrades to the previous
  // heuristic: a node with exactly one control can only be showing that one.
  function controlSlot(el) {
    let owner = null;
    try {
      const nodes = editor.getNodes();
      for (let i = 0; i < nodes.length; i++) {
        const view = area.nodeViews.get(nodes[i].id);
        if (view && view.element && view.element.contains(el)) {
          owner = nodes[i];
          break;
        }
      }
    } catch (_) {
      /* best-effort affordance: never break editing over it */
      return null;
    }
    if (!owner) return null;
    let key = "";
    const wrap =
      typeof el.closest === "function" ? el.closest('[data-testid^="control-"]') : null;
    if (wrap) {
      key = String(wrap.getAttribute("data-testid") || "").slice("control-".length);
    }
    if (!key) {
      const keys = Object.keys(owner.controls || {});
      if (keys.length === 1) key = keys[0];
    }
    return key ? { node: owner, key: key } : null;
  }

  // decorateControl attaches a control's dropdown and field hints the first time
  // the author touches it. Every write is guarded, so it is idempotent across
  // however many times React re-creates the element.
  function decorateControl(ev) {
    const el = ev.target;
    if (!el || el.tagName !== "INPUT") return;
    const slot = controlSlot(el);
    if (!slot) return;
    const hint = CONTROL_HINTS[slot.node.kind + ":" + slot.key];
    if (!hint) return;
    const listId = hint.list ? listIds[hint.list] : "";
    if (listId && el.getAttribute("list") !== listId) el.setAttribute("list", listId);
    if (hint.placeholder && !el.getAttribute("placeholder")) {
      el.setAttribute("placeholder", hint.placeholder);
    }
    if (hint.title && !el.getAttribute("title")) el.setAttribute("title", hint.title);
    if (hint.inputmode && !el.getAttribute("inputmode")) {
      el.setAttribute("inputmode", hint.inputmode);
    }
  }

  container.addEventListener("pointerdown", decorateControl, true);
  container.addEventListener("focusin", decorateControl, true);

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
      if (n.kind === S.TYPES.RELATIVE_DATE) {
        // The three closed-set controls hold readable spellings; the AST wants
        // the tokens. Unrecognised text passes through verbatim so validateAST
        // reports exactly what the author typed into the free-text field.
        //
        // The offset is handed over RAW: the serializer normalises a blank
        // control to 0 and keeps an unparseable token as-is, and duplicating
        // that here would give the editor a second, quieter opinion about what
        // an offset is.
        g.anchor = vocabFromLabel(vocab("ANCHORS"), controlValue(n, "anchor"));
        g.n = n.controls.n ? n.controls.n.value : 0;
        g.unit = vocabFromLabel(vocab("UNITS"), controlValue(n, "unit"));
        g.snap = vocabFromLabel(vocab("SNAPS"), controlValue(n, "snap"));
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

  // controlValue reads one control's text, tolerating a control the node does
  // not carry (a graph read mid-reshape).
  function controlValue(node, key) {
    const control = node.controls ? node.controls[key] : null;
    return control ? control.value : "";
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
        // The relativeDate quartet rides across as-is. A field the stored rule
        // does not carry stays `undefined` so the control opens at its default
        // rather than at an empty string the validator would then flag — but a
        // field it carries EMPTY is passed through and flagged, which is the
        // right answer for a rule that really is missing one.
        anchor: gn.anchor,
        n: gn.n,
        unit: gn.unit,
        snap: gn.snap,
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
    // `is between` arrives ready to author: its ternary shape is a two-item
    // `list` on the right, which is a detail of how the AST stores bounds and
    // not something an author should have to know, so the editor builds it. The
    // scaffold runs here rather than in makeNode because it creates SIBLING
    // nodes and wires, which needs the comparison to be on the canvas first.
    if (kind === S.TYPES.COMPARE && isBoundsOp(node.op)) {
      await reshapeDateOperands(node, "", node.op);
      await area.update("node", node.id);
      fireChange();
    }
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
    container.removeEventListener("pointerdown", decorateControl, true);
    container.removeEventListener("focusin", decorateControl, true);
    listElements.forEach(function (list) {
      if (list.parentNode) list.parentNode.removeChild(list);
    });
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

/*
 * ---- What-if preview: the resolved relative-date bounds --------------------
 *
 * A relative date is a COMPUTATION, not a date. "The start of the year, five
 * years back" is a rule an author can read and still not be sure of — the snap
 * is applied first, calendar units clamp at month end, and `endOf*` means the
 * INCLUSIVE last second. The only way to know they wrote the one they meant is
 * to see what it currently comes out as, which is what these rows show.
 *
 * THE SERVER RESOLVES IT, NOT THIS FILE. Every row below is read off the
 * EvaluateRule response; nothing here computes a date. Re-resolving the operand
 * in the browser would mean a second copy of the clamping, snapping and
 * ISO-week rules, free to disagree with the one that actually decides access —
 * and the whole point of showing the bound is that it is the bound the engine
 * used.
 *
 * AND IT IS RENDERED VERBATIM. The resolved value and the reference instant
 * both arrive in the canonical UTC form and are printed exactly as received,
 * `Z` included. Passing either through a JS Date would restate it in the
 * VIEWER'S zone: a viewer in UTC-5 would read a resolved 2026-01-01T00:00:00Z
 * as the previous calendar YEAR, and would then quite reasonably conclude the
 * rule was broken. A Go test (TestRuleEditorNeverFormatsADateThroughADateObject)
 * scans this file for every spelling of that mistake.
 */

// boundRows turns the response's resolved-bound list into display rows. Each
// row names the operand's position in the rule, spells the four control values
// back as a sentence, and carries the resolved instant VERBATIM.
//
// A bound the server could not resolve at all — an offset that leaves the
// representable year range, or a field outside its vocabulary — comes back with
// an empty `resolved`. That is a real deny and is labelled as one rather than
// left blank, because a blank reads as "still loading".
function boundRows(bounds) {
  if (!Array.isArray(bounds)) return [];
  return bounds.map(function (b, i) {
    const raw = b && typeof b === "object" ? b : {};
    return {
      key: String(raw.path || i),
      path: String(raw.path || ""),
      describe: describeBound(raw),
      resolved: String(raw.resolved || ""),
      unresolved: !raw.resolved,
    };
  });
}

// describeBound spells one relative-date operand as the sentence the four
// controls read as, IN THE ORDER THE SERVER APPLIES THEM: the anchor, then the
// snap, then the offset. Stating the order is the point — an author who reads
// "five years back, then the start of the year" would expect a different date,
// and the two orders really are different functions.
//
// The vocabulary tokens go through the same vocabLabel the controls use, so the
// sentence matches what the node on the canvas is showing.
function describeBound(b) {
  const parts = [vocabLabel(b.anchor)];
  if (b.snap && b.snap !== "none") parts.push("snapped to " + vocabLabel(b.snap));
  const n = String(b.n === undefined || b.n === null ? "" : b.n).trim();
  if (n !== "" && n !== "0") parts.push("offset " + n + " " + vocabLabel(b.unit));
  return parts.join(", ");
}

// noteRows normalises the evaluation's deny-safe notes for display. The one-line
// `message` is rendered by the server so the editor and an Explain trace say the
// same thing about the same observation.
//
// These are why a `false` is trustworthy. A rule that denies because a stored
// value is not a canonical date, or because a `between`'s bounds are inverted,
// looks exactly like a rule that denies because the object does not match — and
// a silent false is how an access-control bug hides.
function noteRows(notes) {
  if (!Array.isArray(notes)) return [];
  return notes.map(function (n, i) {
    const raw = n && typeof n === "object" ? n : {};
    return {
      key: String(raw.kind || "note") + ":" + String(raw.path || "") + ":" + i,
      kind: String(raw.kind || ""),
      message: String(raw.message || ""),
    };
  });
}

// parseJSONField decodes one of the response's JSON-blob fields, tolerating an
// absent or malformed value: the preview's diagnostics must never be the reason
// an evaluation appears to have failed.
function parseJSONField(text) {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (_) {
    return null;
  }
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
    // A relative date is an OPERAND like a variable or a literal — a leaf that
    // produces a value and takes no wires — so it sits with them. It is also
    // reachable without the palette, from a date comparison's mode switch, which
    // is the path an author who is not thinking in nodes will take; the palette
    // entry is what makes it placeable on its own, e.g. to wire into a `between`
    // bound that already exists.
    { group: "Operands", items: [
      { kind: "var", label: "Variable" },
      { kind: "literal", label: "Literal" },
      { kind: "relativeDate", label: "Relative date" },
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
    // The date diagnostics, all three read straight off the response and all
    // three rendered VERBATIM. previewNow is the reference instant the
    // evaluation resolved against, previewBounds is what each relative-date
    // operand became at that instant, and previewNotes is why a false is a
    // false. Nothing here is computed in the browser — see boundRows().
    previewNow: "",
    previewBounds: [],
    previewNotes: [],
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
      this.previewNow = "";
      this.previewBounds = [];
      this.previewNotes = [];
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
      this.previewNow = "";
      this.previewBounds = [];
      this.previewNotes = [];
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
        // The instant and the resolved bounds arrive canonical and are held as
        // received — no reformatting, no Date. previewNow is what every bound
        // below was computed from, so the two are shown together.
        this.previewNow = resp.now || "";
        this.previewBounds = boundRows(parseJSONField(resp.bounds_json));
        this.previewNotes = noteRows(parseJSONField(resp.notes_json));
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
// The date diagnostics' row builders, exported for the same reason. Both are
// pure projections of the response — neither computes a date, and neither
// reformats one.
window.boundRows = boundRows;
window.noteRows = noteRows;
