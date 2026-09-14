// The instruction floor, mirrored from the server so the form can say what is
// missing before it submits. store/instruction.go is the authority; this is
// the same rule stated twice on purpose, because a button that submits into a
// 400 is worse than a button that explains itself.

export const MIN_INSTRUCTION_CHARS = 20;
export const MIN_INSTRUCTION_WORDS = 4;

const words = (s: string) => s.trim().split(/\s+/).filter(Boolean);

/** instructionProblem returns what is wrong with the instruction, or "". */
export function instructionProblem(title: string, task: string): string {
  const t = words(task).join(" ");
  if (!t) return "Say what this loop should do.";
  if (title.trim() && words(title).join(" ").toLowerCase() === t.toLowerCase()) {
    return "This repeats the name. Say what to do, not what to call it.";
  }
  if ([...t].length < MIN_INSTRUCTION_CHARS) {
    return `A bit more detail: ${[...t].length} of ${MIN_INSTRUCTION_CHARS} characters.`;
  }
  if (words(t).length < MIN_INSTRUCTION_WORDS) {
    return `A bit more detail: ${words(t).length} of ${MIN_INSTRUCTION_WORDS} words.`;
  }
  return "";
}
