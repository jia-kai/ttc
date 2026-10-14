package session

import "ttc/internal/prompts"

const systemTemplate = prompts.System + "\n" + prompts.Coding + "\n" + prompts.Main

// childSystemTemplate keeps child behavior stable without embedding an actor ID.
const childSystemTemplate = prompts.System + "\n" + prompts.Coding + "\n" + prompts.Child

// Aside scope is supplied by the separate developer message, without coding or
// main-only workflows that require tools absent from the read-only registry.
const asideSystemTemplate = prompts.System
