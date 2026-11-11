package session

import "ttc/internal/prompts"

const systemTemplate = prompts.System + "\n" + prompts.Coding + "\n" + prompts.Main

// childSystemTemplate keeps child behavior stable without embedding an actor ID.
const childSystemTemplate = prompts.System + "\n" + prompts.Coding + "\n" + prompts.Child
