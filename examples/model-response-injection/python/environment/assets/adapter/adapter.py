import sys
from agent import Agent


def initialize():
    print('application initialized', file=sys.stderr)
    return Agent()


def invoke(agent, input):
    return agent.invoke(input)


def close(agent):
    agent.close()
    print('application closed', file=sys.stderr)
