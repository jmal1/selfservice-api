if type != "object" then
  error("worker Deployment must be an object")
elif (.spec | type) != "object" then
  error("worker Deployment .spec must be an object")
elif (.spec.replicas | type) != "number" then
  error("worker Deployment .spec.replicas must be a number")
elif (.spec.replicas | floor) != .spec.replicas then
  error("worker Deployment .spec.replicas must be an integer")
elif (.spec.replicas != 1 and .spec.replicas != 2) then
  # 1 = pre-Wave-D rollback target / live during the first 1→2 apply.
  # 2 = Wave D steady state. Candidate manifests still require exactly 2.
  error("worker Deployment .spec.replicas must equal 1 or 2")
else
  .spec.replicas
end
