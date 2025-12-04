#
#  mmmm mmmmmmm m    m mmmmm
# #"   "   #    #    # #    #
# "#mmm    #    #    # #mmmm"
#     "#   #    #    # #    #
# "mmm#"   #    "mmmm" #mmmm"
#

FROM debian:trixie-slim AS base

RUN date | tee /date

FROM base AS kaniko-warmer

FROM base AS kaniko-executor

FROM base AS kaniko-debug

FROM base AS kaniko-debug-2

FROM base AS kaniko-slim
