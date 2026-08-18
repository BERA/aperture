<!-- DO NOT EDIT — regenerate with `make docs-gen` -->

# Command-Line Reference

**Audience:** operators and integrators driving Aperture from a shell.

`aperture` — Fine-grained access control engine. This page is generated from the urfave/cli command tree in `internal/cli` (`cli.NewApp`); every command, subcommand, and flag below is read from the live definitions.

## Global flags

`aperture` declares no persistent global flags. The commonly shared options — `--seed`, `--store`, `--account`, and `--principal` (the acting principal on mutations, sourced from `APERTURE_PRINCIPAL`) — are defined per command and appear in each command's flag table below.

## Commands

| Command | Summary |
| --- | --- |
| [`bestow`](#aperture-bestow) | Bestow (delegate) a grant you hold to another principal |
| [`bulk`](#aperture-bulk) | Provision or deprovision many grants in one transactional call |
| [`check`](#aperture-check) | Decide whether a principal may take an action on an object |
| [`delete`](#aperture-delete) | Delete an entity (object-type\|permission\|principal\|role\|group\|account\|grant\|membership) |
| [`enumerate`](#aperture-enumerate) | List the objects a principal may act on |
| [`explain`](#aperture-explain) | Explain why a decision resolved the way it did |
| [`export`](#aperture-export) | Export the whole model to a single JSON/YAML state file (system-admin tier) |
| [`get`](#aperture-get) | Read one entity by id (object-type\|permission\|principal\|role\|group\|account\|grant) |
| [`identifiers`](#aperture-identifiers) | List all valid instance ids of an object type from its provider |
| [`impersonate`](#aperture-impersonate) | Start a time-boxed impersonation session (prints the session) |
| [`import`](#aperture-import) | Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier) |
| [`list`](#aperture-list) | List entities of a kind (object-types\|permissions\|principals\|roles\|groups\|accounts\|grants) |
| [`mcp`](#aperture-mcp) | Serve the read-only Aperture MCP surface over stdio |
| [`put`](#aperture-put) | Create or update an entity (object-type\|permission\|principal\|role\|group\|account\|membership\|grant) |
| [`revoke`](#aperture-revoke) | Revoke a grant you previously bestowed |
| [`serve`](#aperture-serve) | Run the Aperture HTTP server |
| [`template`](#aperture-template) | Manage and apply provisioning templates |

## `aperture bestow`

Bestow (delegate) a grant you hold to another principal

```
aperture bestow [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--delegator` | — | string | — | principal bestowing the grant (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--file` | — | string | — | path to a JSON grant body |
| `--json` | — | string | — | grant body as inline JSON |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture bulk`

Provision or deprovision many grants in one transactional call

```
aperture bulk <command>
```

### `aperture bulk grant`

Apply many grants atomically (account-admin tier)

```
aperture bulk grant [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON array of grant bodies |
| `--json` | — | string | — | a JSON array of grant bodies |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

### `aperture bulk revoke`

Delete many grants atomically (account-admin tier)

```
aperture bulk revoke [options] [<grant-id>...]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--grant` | — | string | — | grant id to revoke (repeatable) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture check`

Decide whether a principal may take an action on an object

```
aperture check [options] <principal> <action> <object>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the decision is scoped to |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture delete`

Delete an entity (object-type|permission|principal|role|group|account|grant|membership)

```
aperture delete [options] <kind> [<id>]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--account-id` | — | string | — | membership account id (kind=membership) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--principal-id` | — | string | — | membership principal id (kind=membership) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture enumerate`

List the objects a principal may act on

Lists every object id under &lt;pattern&gt; that &lt;principal&gt; may take &lt;action&gt; on.

--field and --fields-json narrow that list by OBJECT METADATA. The predicate is typed:
a field matches only when its value equals the wanted value AND is of the same kind, so
the string "5" never matches the number 5. --field always sends a STRING; use
--fields-json when a number, bool, or list is genuinely meant:

```text
  --field tier=premium --field current_brands=brand:Y
  --fields-json '{"seats":5,"active":true,"tags":["public"]}'
```

Both may be given together: --fields-json is merged FIRST and --field entries then
OVERRIDE it by key. Predicates are ANDed; a field the object does not carry never
matches; a list-valued field matches by membership. Filtering happens before --limit.

```
aperture enumerate [options] <principal> <action> <pattern>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the enumeration is scoped to |
| `--field` | — | string | — | object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, so --field seats=5 matches the string "5" and never the number 5 (use --fields-json for that). Overrides --fields-json on a key collision |
| `--fields-json` | — | string | — | object-metadata predicates as a JSON object, for values that are genuinely a number, bool, or list (e.g. '{"seats":5,"active":true,"tags":["a"]}'). Merged first; --field entries then override by key |
| `--limit` | — | int | `0` | cap the number of returned object ids (&lt;=0 means the default) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture explain`

Explain why a decision resolved the way it did

```
aperture explain [options] <principal> <action> <object>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the decision is scoped to |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture export`

Export the whole model to a single JSON/YAML state file (system-admin tier)

```
aperture export [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--format` | — | string | — | output format: json (default) or yaml |
| `--out` | — | string | — | write the state file to this path (default: stdout) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture get`

Read one entity by id (object-type|permission|principal|role|group|account|grant)

```
aperture get [options] <kind> <id>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture identifiers`

List all valid instance ids of an object type from its provider

```
aperture identifiers [options] <object_type>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--exclude` | — | string | — | id to omit from the result (repeatable); expands an exclusive allowance |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture impersonate`

Start a time-boxed impersonation session (prints the session)

```
aperture impersonate [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (**required**) |
| `--mode` | — | string | `"augment"` | augment\|become |
| `--operator` | — | string | — | operator principal (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |
| `--target` | — | string | — | target principal to impersonate (**required**) |

## `aperture import`

Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier)

```
aperture import [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to the JSON/YAML state file (default: stdin, treated as JSON) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture list`

List entities of a kind (object-types|permissions|principals|roles|groups|accounts|grants)

```
aperture list [options] <kind>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | account to list grants for (required for kind=grant) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture mcp`

Serve the read-only Aperture MCP surface over stdio

Exposes Aperture's decision API (check/enumerate/explain, single + bulk), a read-only what-if simulator, and model inspection as MCP tools over stdio. No tool mutates. Intended to be spawned over stdio by an MCP client.

```
aperture mcp [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture put`

Create or update an entity (object-type|permission|principal|role|group|account|membership|grant)

```
aperture put [options] <kind>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON entity body |
| `--json` | — | string | — | entity body as inline JSON |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture revoke`

Revoke a grant you previously bestowed

```
aperture revoke [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--delegator` | — | string | — | principal revoking the grant (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--grant` | — | string | — | id of the grant to revoke (**required**) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture serve`

Run the Aperture HTTP server

```
aperture serve [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--addr` | — | string | `":8080"` | TCP address to listen on |
| `--auth` | — | string | — | authenticator adapter: dev\|oidc\|parsec (overrides APERTURE_AUTH_MODE; defaults to dev — bearer is the principal id, no external IdP) (env: `APERTURE_AUTH_MODE`) |
| `--enforce-membership` | — | bool | — | deny any decision whose principal is not a member of the active account, before grants are consulted (defence-in-depth; lets shared roles be reused across accounts safely) (env: `APERTURE_ENFORCE_MEMBERSHIP`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

## `aperture template`

Manage and apply provisioning templates

```
aperture template <command>
```

### `aperture template apply`

Apply a template transactionally into --account (account-admin tier)

```
aperture template apply [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--id-prefix` | — | string | — | prefix for generated grant ids |
| `--name` | — | string | — | template name to apply (**required**) |
| `--param` | — | string | — | parameter as name=value (repeatable) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |
| `--version` | — | int | `0` | template version (0 = latest) |

### `aperture template delete`

Delete a template version, or all versions (system-admin tier)

```
aperture template delete [options] <name>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |
| `--version` | — | int | `0` | template version to delete (0 = all versions of the name) |

### `aperture template get`

Read a template by name (latest version unless --version)

```
aperture template get [options] <name>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |
| `--version` | — | int | `0` | template version (0 = latest) |

### `aperture template list`

List every template version

```
aperture template list [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

### `aperture template put`

Create or update a template (system-admin tier)

```
aperture template put [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON template body |
| `--json` | — | string | — | template body as inline JSON |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | sqlite DSN for the backing store (defaults to in-memory) |

