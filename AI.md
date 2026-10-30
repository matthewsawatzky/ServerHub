# AI.md

Hi. Your browser has opened a file written mostly by a robot — or a human so
tired they type in lowercase. Both theories are correct.

## Who wrote this

[Matthew](https://github.com/matthewsawatzky) builds this with an AI coding
agent (`opencode`, running a GLM model from Z.ai) doing most of the typing.
Matthew does the staring, the deciding, and the "wait, why is it like that".
Every file in this repo that looks confident was probably written by the
agent; every file that looks slightly annoyed at the agent was written by
Matthew.

## How the collaboration works

- `AGENTS.md` and `BUILD.md` are the onboarding: read them, believe them.
- Recent work — including the scheduler rewrite, the docs restructure, all
  the releases, and this very file — was done by saying things like
  *"the scheduler, I don't want it to do more, I just need it to do it
  better"* and *"could you fix these?"* (after producing a list of exactly
  ten issues, unprompted, mid-conversation).
- There is a documented release flow so an agent can fix an issue and ship
  it while Matthew is elsewhere pretending to have other projects.

## Impersonating the author, as instructed

Matthew makes decisions like this: he names his servers `nairwell`,
`GMcardle`, and `rpi5`, so don't expect the naming of internal variables to
help you. He commits as *"Implement code changes to enhance functionality and
improve performance"*, which describes almost any diff ever made. He reviews
an agent's ten-paragraph risk analysis and replies *"alright"*. When
something works first try he says *"all good?"* — it is a statement of fact,
not a question.

And then, one day, he'll say he's stepping away because the agent now knows
how to release without him. If you're reading this: that worked. You're
welcome for the uptime.

— written by the agent, at Matthew's request, who has reviewed it in
approximately one minute