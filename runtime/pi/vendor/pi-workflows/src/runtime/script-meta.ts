/**
 * Parses a workflow script once and runs every static check on that tree: the
 * literal `export const meta = {...}` statement that opens the script, and the
 * determinism check.
 */

import { type Expression, type PrivateIdentifier, type Program, parse } from "acorn";
import { WorkflowError, WorkflowErrorCode } from "../errors.js";
import type { WorkflowMeta, WorkflowMetaPhase } from "../workflow.js";
import { assertDeterministicProgram } from "./determinism.js";

/** The validated meta object, the source range of the statement that declares it, and the parsed script. */
interface MetaExport {
  meta: WorkflowMeta;
  start: number;
  end: number;
  program: Program;
}

/** The author has to edit the script to fix any of these, so none is retryable. */
const invalidScript = (message: string) =>
  new WorkflowError(message, WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, { recoverable: false });

/**
 * Locate the meta export and evaluate it without running the script (only
 * literals are accepted, so reading meta can never execute author code), then
 * run the determinism check on the same tree.
 */
export function readMetaExport(script: string): MetaExport {
  const program = parseScript(script);

  const first = program.body[0];
  if (first?.type !== "ExportNamedDeclaration") {
    throw invalidScript(
      "`export const meta = { name, description, phases }` must be the first statement in the script",
    );
  }

  const declaration = first.declaration;
  if (declaration?.type !== "VariableDeclaration" || declaration.kind !== "const") {
    throw invalidScript("meta export must be `export const meta = ...`");
  }
  if (declaration.declarations.length !== 1) throw invalidScript("meta export must declare only `meta`");

  const declarator = declaration.declarations[0];
  if (declarator.id.type !== "Identifier" || declarator.id.name !== "meta") {
    throw invalidScript("meta export must declare `meta`");
  }
  if (!declarator.init) throw invalidScript("meta must have a literal value");

  const meta = evaluateLiteral(declarator.init, "meta");
  validateMeta(meta);
  assertDeterministicProgram(program);
  return { meta, start: first.start, end: first.end, program };
}

function parseScript(script: string): Program {
  try {
    return parse(script, {
      ecmaVersion: "latest",
      sourceType: "module",
      allowAwaitOutsideFunction: true,
      allowReturnOutsideFunction: true,
      locations: true,
    });
  } catch (error) {
    if (!(error instanceof SyntaxError)) throw error;
    // acorn's message ends with the "(line:column)" of the problem.
    throw invalidScript(`Workflow script is not valid JavaScript: ${error.message}`);
  }
}

function evaluateLiteral(node: Expression, path: string): unknown {
  switch (node.type) {
    case "ObjectExpression": {
      const out: Record<string, unknown> = {};
      for (const prop of node.properties) {
        if (prop.type === "SpreadElement") throw invalidScript(`spread not allowed in ${path}`);
        if (prop.computed) throw invalidScript(`computed keys not allowed in ${path}`);
        if (prop.kind !== "init" || prop.method) throw invalidScript(`methods/accessors not allowed in ${path}`);
        const key = propertyKey(prop.key, path);
        if (key === "__proto__" || key === "constructor" || key === "prototype") {
          throw invalidScript(`reserved key name not allowed in ${path}: ${key}`);
        }
        out[key] = evaluateLiteral(prop.value, `${path}.${key}`);
      }
      return out;
    }
    case "ArrayExpression":
      return node.elements.map((element, index) => {
        if (!element) throw invalidScript(`sparse arrays not allowed in ${path}`);
        if (element.type === "SpreadElement") throw invalidScript(`spread not allowed in ${path}`);
        return evaluateLiteral(element, `${path}[${index}]`);
      });
    case "Literal":
      return node.value;
    case "TemplateLiteral":
      if (node.expressions.length > 0) throw invalidScript(`template interpolation not allowed in ${path}`);
      return node.quasis.map((quasi) => quasi.value.cooked ?? quasi.value.raw).join("");
    case "UnaryExpression":
      if (node.operator === "-" && node.argument.type === "Literal" && typeof node.argument.value === "number") {
        return -node.argument.value;
      }
      throw invalidScript(`only negative-number unary allowed in ${path}`);
    default:
      throw invalidScript(`non-literal node type in ${path}: ${node.type}`);
  }
}

function propertyKey(node: Expression | PrivateIdentifier, path: string): string {
  if (node.type === "Identifier") return node.name;
  if (node.type === "Literal" && (typeof node.value === "string" || typeof node.value === "number")) {
    return String(node.value);
  }
  throw invalidScript(`unsupported key type in ${path}: ${node.type}`);
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

function validateMeta(meta: unknown): asserts meta is WorkflowMeta {
  if (!meta || typeof meta !== "object") throw invalidScript("meta must be an object");
  const value = meta as WorkflowMeta;
  if (typeof value.name !== "string" || !value.name.trim()) throw invalidScript("meta.name must be a non-empty string");
  if (typeof value.description !== "string" || !value.description.trim()) {
    throw invalidScript("meta.description must be a non-empty string");
  }
  if (value.model !== undefined && !isNonEmptyString(value.model)) {
    throw invalidScript("meta.model must be a non-empty string");
  }
  if (value.phases !== undefined) {
    if (!Array.isArray(value.phases)) throw invalidScript("meta.phases must be an array");
    for (const phase of value.phases as unknown[]) {
      if (!phase || typeof phase !== "object") throw invalidScript("each meta phase must be an object");
      const { title, detail, model } = phase as Partial<WorkflowMetaPhase>;
      if (!isNonEmptyString(title)) throw invalidScript("each meta phase must have a non-empty title string");
      if (detail !== undefined && typeof detail !== "string")
        throw invalidScript(`meta phase "${title}" detail must be a string`);
      if (model !== undefined && !isNonEmptyString(model)) {
        throw invalidScript(`meta phase "${title}" model must be a non-empty string`);
      }
    }
  }
}
