import sys
from agent import Agent


class Adapter:
    def __init__(self):
        self.agent = Agent()
        print('application initialized', file=sys.stderr)

    def invoke(self, input):
        return self.agent.invoke(input)

    def close(self):
        self.agent.close()
        print('application closed', file=sys.stderr)


def initialize(options):
    return Adapter()
