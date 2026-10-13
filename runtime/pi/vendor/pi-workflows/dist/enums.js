/** Classifies a workflow capability by its runtime and documentation role. */
export var CapabilityClassification;
(function (CapabilityClassification) {
    CapabilityClassification["RUNTIME_GLOBAL"] = "runtime-global";
    CapabilityClassification["SUBAGENTS_TOOL_INPUT"] = "subagents-tool-input";
    CapabilityClassification["SCRIPT_CONTRACT"] = "script-contract";
    CapabilityClassification["COMPATIBILITY_BEHAVIOR"] = "compatibility-behavior";
    CapabilityClassification["INTERNAL_SUBSTRATE"] = "internal-substrate";
    CapabilityClassification["DYNAMIC_REFERENCE"] = "dynamic-reference";
})(CapabilityClassification || (CapabilityClassification = {}));
/** Declares whether workflow authors should use a capability. */
export var CapabilitySupport;
(function (CapabilitySupport) {
    CapabilitySupport["SUPPORTED"] = "supported";
    CapabilitySupport["COMPATIBILITY"] = "compatibility";
    CapabilitySupport["INTERNAL"] = "internal";
})(CapabilitySupport || (CapabilitySupport = {}));
/** Names the subsystem that owns a capability's behavior. */
export var CapabilityOrigin;
(function (CapabilityOrigin) {
    CapabilityOrigin["PROJECT"] = "project";
    CapabilityOrigin["TOOL_ADAPTER"] = "tool-adapter";
    CapabilityOrigin["VM_REALM"] = "vm-realm";
    CapabilityOrigin["LIVE_CONFIGURATION"] = "live-configuration";
})(CapabilityOrigin || (CapabilityOrigin = {}));
/** Severity carried by capability-alignment diagnostics. */
export var DiagnosticSeverity;
(function (DiagnosticSeverity) {
    DiagnosticSeverity["ERROR"] = "error";
    DiagnosticSeverity["WARNING"] = "warning";
})(DiagnosticSeverity || (DiagnosticSeverity = {}));
