import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { CapabilityClassification, CapabilitySupport } from "./enums.js";
import {
  CAPABILITY_DETAIL_PATH,
  type OptionDescriptor,
  type StaticCapabilityFact,
  WORKFLOW_CAPABILITY_CONTRACT,
} from "./workflow-capability-contract.js";

const GENERATED_MARKER = "<!-- GENERATED from WORKFLOW_CAPABILITY_CONTRACT; do not edit by hand. -->";
const TABLE_START = "<!-- BEGIN GENERATED SUPPORTED WORKFLOW CAPABILITIES -->";
const TABLE_END = "<!-- END GENERATED SUPPORTED WORKFLOW CAPABILITIES -->";

/** Package-relative compact capability index generated from the contract. */
const CAPABILITY_INDEX_PATH = "skills/workflow-authoring/references/capabilities.md";

/** Documents that embed the byte-identical supported-capability table. */
export const CAPABILITY_TABLE_PUBLICATION_PATHS = [
  CAPABILITY_INDEX_PATH,
  "README.md",
  "docs/workflow-authoring.md",
] as const;
/** All generated capability publication surfaces checked for drift. */
const CAPABILITY_PUBLICATION_PATHS = [...CAPABILITY_TABLE_PUBLICATION_PATHS, CAPABILITY_DETAIL_PATH] as const;

function escapeTable(value: string): string {
  return value.replaceAll("|", "\\|").replaceAll("\n", " ");
}

function display(value: string | null): string {
  return value === null ? "—" : `\`${escapeTable(value)}\``;
}

function optionText(option: OptionDescriptor): string {
  const required = option.optional ? "optional" : "required";
  const defaultValue = option.default === null ? "" : `; default: ${option.default}`;
  const constraints = option.constraints.length === 0 ? "" : `; ${option.constraints.join("; ")}`;
  const dynamic = option.dynamicReference === null ? "" : `; dynamic reference: ${option.dynamicReference}`;
  return `\`${option.name}\`: ${option.type} (${required}${defaultValue}${constraints}${dynamic})`;
}

function compactOptions(fact: StaticCapabilityFact): string {
  if (!fact.options) return "—";
  return fact.options.options
    .map((option) => {
      const optionality = option.optional ? "optional" : "required";
      const defaultValue = option.default === null ? "" : `; default: ${option.default}`;
      return `\`${escapeTable(option.name)}\`: ${escapeTable(option.type)} (${optionality}${escapeTable(defaultValue)})`;
    })
    .join("<br>");
}

function publishedFacts(): readonly StaticCapabilityFact[] {
  return WORKFLOW_CAPABILITY_CONTRACT.projectStaticReferenceFacts().filter(
    (fact) =>
      fact.support === CapabilitySupport.SUPPORTED &&
      (fact.classification === CapabilityClassification.RUNTIME_GLOBAL ||
        fact.classification === CapabilityClassification.SUBAGENTS_TOOL_INPUT),
  );
}

/** The byte-identical generated block embedded in every public documentation surface. */
export function renderSupportedCapabilityTable(): string {
  const rows = publishedFacts().map(
    (fact) =>
      `| ${escapeTable(fact.label)} | ${fact.classification} | ${display(fact.signature)} | ${compactOptions(fact)} |`,
  );
  return `${TABLE_START}
| Name | Classification | Signature | Options and defaults |
| --- | --- | --- | --- |
${rows.join("\n")}
${TABLE_END}`;
}

function replaceSupportedCapabilityTable(document: string): string | null {
  const start = document.indexOf(TABLE_START);
  const end = document.indexOf(TABLE_END, start + TABLE_START.length);
  if (start < 0 || end < 0 || document.indexOf(TABLE_START, start + TABLE_START.length) >= 0) return null;
  const after = end + TABLE_END.length;
  return `${document.slice(0, start)}${renderSupportedCapabilityTable()}${document.slice(after)}`;
}

/** Regenerates only contract-owned content, preserving hand-written prose around marked blocks. */
export function writeWorkflowCapabilityPublications(root: string): void {
  for (const path of CAPABILITY_TABLE_PUBLICATION_PATHS) {
    const absolutePath = join(root, path);
    if (path === CAPABILITY_INDEX_PATH) {
      writeFileSync(absolutePath, renderWorkflowCapabilityReference());
      continue;
    }
    const source = readFileSync(absolutePath, "utf8");
    const refreshed = replaceSupportedCapabilityTable(source);
    if (refreshed === null) throw new Error(`Missing or duplicate generated capability-table anchors in ${path}.`);
    writeFileSync(absolutePath, refreshed);
  }
  writeFileSync(join(root, CAPABILITY_DETAIL_PATH), renderWorkflowCapabilityDetails());
}

/** Returns every stale surface in stable publication order. Overrides let tests check edited content without writing files. */
export function checkWorkflowCapabilityPublications(
  root: string,
  overrides: Readonly<Partial<Record<(typeof CAPABILITY_PUBLICATION_PATHS)[number], string>>> = {},
): string[] {
  const stale: string[] = [];
  for (const path of CAPABILITY_TABLE_PUBLICATION_PATHS) {
    const actual = overrides[path] ?? readFileSync(join(root, path), "utf8");
    if (path === CAPABILITY_INDEX_PATH) {
      if (actual !== renderWorkflowCapabilityReference()) stale.push(path);
      continue;
    }
    const refreshed = replaceSupportedCapabilityTable(actual);
    if (refreshed === null || refreshed !== actual) stale.push(path);
  }
  const details = overrides[CAPABILITY_DETAIL_PATH] ?? readFileSync(join(root, CAPABILITY_DETAIL_PATH), "utf8");
  if (details !== renderWorkflowCapabilityDetails()) stale.push(CAPABILITY_DETAIL_PATH);
  return stale;
}

function detail(fact: StaticCapabilityFact): string {
  const lines = [
    `<a id="${fact.reference.split("#")[1]}"></a>`,
    `## ${fact.label}`,
    "",
    `- Classification: \`${fact.classification}\``,
    `- Support: \`${fact.support}\``,
    `- Signature: ${display(fact.signature)}`,
  ];
  if (fact.options) {
    lines.push(
      ...fact.options.positional.map((argument) => `- Argument ${optionText(argument)}`),
      `- Option shape: \`${fact.options.id}\``,
      ...fact.options.options.map((option) => `- ${optionText(option)}`),
    );
  }
  if (fact.constraints.length > 0) lines.push(...fact.constraints.map((constraint) => `- Constraint: ${constraint}`));
  if (fact.dynamicReference) {
    lines.push(
      `- Dynamic reference owner: \`${fact.dynamicReference.owner}\``,
      `- Item shape: \`${fact.dynamicReference.itemShape}\``,
      `- Live lookup: \`${fact.dynamicReference.connection}\``,
      "- Live values are intentionally absent from this static reference.",
    );
  }
  return `${lines.join("\n")}\n`;
}

/** Render the compact index that routes exact lookups to the exhaustive details. */
export function renderWorkflowCapabilityReference(): string {
  const { definition } = WORKFLOW_CAPABILITY_CONTRACT;
  return `${GENERATED_MARKER}
# Workflow capability index

Contract format: \`${definition.versions.format.version}\`<br>
Contract content / skill / extension: \`${definition.versions.content.version}\`

This compact generated index covers supported runtime globals and \`subagents\` tool inputs. For constraints, compatibility behavior, internal boundaries, and dynamic-reference ownership, follow the [exhaustive generated facts](capability-details.md).

## Supported capability index

${renderSupportedCapabilityTable()}
`;
}

/** Render every contract fact under a stable anchor, leaving live catalogues as dynamic references. */
export function renderWorkflowCapabilityDetails(): string {
  const { definition } = WORKFLOW_CAPABILITY_CONTRACT;
  const facts = WORKFLOW_CAPABILITY_CONTRACT.projectStaticReferenceFacts();

  return `${GENERATED_MARKER}
# Exhaustive workflow capability facts

Contract format: \`${definition.versions.format.version}\`<br>
Contract content / skill / extension: \`${definition.versions.content.version}\`

Every exact fact below is projected from the installed extension's capability contract. Explanatory judgment belongs in the hand-written references next to this file.

${facts.map(detail).join("\n")}`;
}
