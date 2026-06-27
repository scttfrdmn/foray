# Copyright 2026 Scott Friedman
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# foray worker — the one Python boundary (ARCHITECTURE.md §6.7). A FastAPI server
# that receives a serialized nnsight intervention graph from forayd, runs it
# interleaved with the forward pass on the session's ephemeral GPU, and returns
# *references* to saved values in S3 (in-region) — never tensors. See README.md.
