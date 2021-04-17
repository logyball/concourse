package exec_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/exec"
	"github.com/concourse/concourse/atc/exec/build"
	"github.com/concourse/concourse/atc/exec/execfakes"
	"github.com/concourse/concourse/atc/runtime"
	"github.com/concourse/concourse/atc/runtime/runtimetest"
	"github.com/concourse/concourse/atc/worker"
	"github.com/concourse/concourse/tracing"
	"github.com/concourse/concourse/vars"
	"github.com/onsi/gomega/gbytes"
	"go.opentelemetry.io/otel/oteltest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("TaskStep", func() {
	var (
		ctx    context.Context
		cancel func()

		stdoutBuf *gbytes.Buffer
		stderrBuf *gbytes.Buffer

		fakePool     *execfakes.FakePool
		fakeStreamer *execfakes.FakeStreamer

		fakeDelegate *execfakes.FakeTaskDelegate

		fakeDelegateFactory *execfakes.FakeTaskDelegateFactory

		taskPlan *atc.TaskPlan

		state exec.RunState
		repo  *build.Repository

		taskStep exec.Step
		stepOk   bool
		stepErr  error

		containerMetadata = db.ContainerMetadata{
			WorkingDirectory: "some-artifact-root",
			Type:             db.ContainerTypeTask,
			StepName:         "some-step",
		}

		stepMetadata = exec.StepMetadata{
			TeamID:  123,
			BuildID: 1234,
			JobID:   12345,
		}

		planID = atc.PlanID("42")

		expectedOwner = db.NewBuildStepContainerOwner(stepMetadata.BuildID, planID, stepMetadata.TeamID)
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())

		stdoutBuf = gbytes.NewBuffer()
		stderrBuf = gbytes.NewBuffer()

		fakeStreamer = new(execfakes.FakeStreamer)

		fakeDelegate = new(execfakes.FakeTaskDelegate)
		fakeDelegate.StdoutReturns(stdoutBuf)
		fakeDelegate.StderrReturns(stderrBuf)

		fakeDelegate.StartSpanReturns(ctx, tracing.NoopSpan)

		fakeDelegateFactory = new(execfakes.FakeTaskDelegateFactory)
		fakeDelegateFactory.TaskDelegateReturns(fakeDelegate)

		state = exec.NewRunState(noopStepper, vars.StaticVariables{"source-param": "super-secret-source"}, false)
		repo = state.ArtifactRepository()

		taskPlan = &atc.TaskPlan{
			Name:       "some-task",
			Privileged: false,
			VersionedResourceTypes: atc.VersionedResourceTypes{
				{
					ResourceType: atc.ResourceType{
						Name:   "custom-resource",
						Type:   "custom-type",
						Source: atc.Source{"some-custom": "((source-param))"},
						Params: atc.Params{"some-custom": "param"},
					},
					Version: atc.Version{"some-custom": "version"},
				},
			},
		}
	})

	JustBeforeEach(func() {
		plan := atc.Plan{
			ID:   planID,
			Task: taskPlan,
		}

		// stuff stored on task step still
		taskStep = exec.NewTaskStep(
			plan.ID,
			*plan.Task,
			atc.ContainerLimits{},
			stepMetadata,
			containerMetadata,
			nil,
			fakePool,
			fakeStreamer,
			fakeDelegateFactory,
		)

		stepOk, stepErr = taskStep.Run(ctx, state)
	})

	expectWorkerSpecResourceTypeUnset := func() {
		Expect(fakePool.FindOrSelectWorkerCallCount()).To(Equal(1))
		_, _, _, workerSpec, _, _ := fakePool.FindOrSelectWorkerArgsForCall(0)
		Expect(workerSpec.ResourceType).To(Equal(""))
	}

	Context("when the plan has a config", func() {
		var chosenWorker *runtimetest.Worker
		var chosenContainer *runtimetest.WorkerContainer

		BeforeEach(func() {
			cpu := atc.CPULimit(1024)
			memory := atc.MemoryLimit(1024)

			taskPlan.Config = &atc.TaskConfig{
				Platform: "some-platform",
				Limits: &atc.ContainerLimits{
					CPU:    &cpu,
					Memory: &memory,
				},
				Params: atc.TaskEnv{
					"SECURE": "secret-task-param",
				},
				Run: atc.TaskRunConfig{
					Path: "ls",
					Args: []string{"some", "args"},
				},
			}

			chosenWorker = runtimetest.NewWorker("worker").
				WithContainer(
					expectedOwner,
					runtimetest.NewContainer().WithProcess(
						runtime.ProcessSpec{
							ID:   "task",
							Path: "ls",
							Args: []string{"some", "args"},
							Dir:  "some-artifact-root",
							TTY: &runtime.TTYSpec{
								WindowSize: runtime.WindowSize{
									Columns: 500,
									Rows:    500,
								},
							},
						},
						runtimetest.ProcessStub{Attachable: true},
					),
					nil,
				)
			chosenContainer = chosenWorker.Containers[0]
			fakePool = new(execfakes.FakePool)
			fakePool.FindOrSelectWorkerReturns(chosenWorker, nil)
		})

		Context("before running the task", func() {
			BeforeEach(func() {
				chosenContainer.ProcessDefs[0].Stub.Do = func(_ context.Context, _ *runtimetest.Process) error {
					defer GinkgoRecover()
					Expect(fakeDelegate.InitializingCallCount()).To(Equal(1))

					return nil
				}
			})

			It("invokes the delegate's Initializing callback", func() {
				// validate the process actually ran
				Expect(chosenContainer.Processes).To(HaveLen(1))
			})
		})

		Describe("worker selection", func() {
			var ctx context.Context
			var workerSpec worker.Spec

			JustBeforeEach(func() {
				Expect(fakePool.FindOrSelectWorkerCallCount()).To(Equal(1))
				ctx, _, _, workerSpec, _, _ = fakePool.FindOrSelectWorkerArgsForCall(0)
			})

			It("doesn't enforce a timeout", func() {
				_, ok := ctx.Deadline()
				Expect(ok).To(BeFalse())
			})

			It("emits a SelectedWorker event", func() {
				Expect(fakeDelegate.SelectedWorkerCallCount()).To(Equal(1))
				_, workerName := fakeDelegate.SelectedWorkerArgsForCall(0)
				Expect(workerName).To(Equal("worker"))
			})

			Context("when tags are configured", func() {
				BeforeEach(func() {
					taskPlan.Tags = atc.Tags{"plan", "tags"}
				})

				It("creates a worker spec with the tags", func() {
					Expect(workerSpec.Tags).To(Equal([]string{"plan", "tags"}))
				})
			})

			Context("when selecting a worker fails", func() {
				BeforeEach(func() {
					fakePool.FindOrSelectWorkerReturns(nil, errors.New("nope"))
				})

				It("returns an err", func() {
					Expect(stepErr).To(MatchError(ContainSubstring("nope")))
				})
			})
		})

		It("sets the config on the TaskDelegate", func() {
			Expect(fakeDelegate.SetTaskConfigCallCount()).To(Equal(1))
			actualTaskConfig := fakeDelegate.SetTaskConfigArgsForCall(0)
			Expect(actualTaskConfig).To(Equal(*taskPlan.Config))
		})

		Context("when privileged", func() {
			BeforeEach(func() {
				taskPlan.Privileged = true
			})

			It("marks the container's image spec as privileged", func() {
				Expect(chosenContainer.Spec.ImageSpec.Privileged).To(BeTrue())
			})
		})

		Context("when a timeout is configured", func() {
			BeforeEach(func() {
				taskPlan.Timeout = "1ms"

				chosenContainer.ProcessDefs[0].Stub.Do = func(ctx context.Context, _ *runtimetest.Process) error {
					select {
					case <-ctx.Done():
						return fmt.Errorf("wrapped: %w", ctx.Err())
					case <-time.After(100 * time.Millisecond):
						return nil
					}
				}
			})

			It("fails without error", func() {
				Expect(stepOk).To(BeFalse())
				Expect(stepErr).To(BeNil())
			})

			It("emits an Errored event", func() {
				Expect(fakeDelegate.ErroredCallCount()).To(Equal(1))
				_, status := fakeDelegate.ErroredArgsForCall(0)
				Expect(status).To(Equal(exec.TimeoutLogMessage))
			})

			Context("when the timeout is bogus", func() {
				BeforeEach(func() {
					taskPlan.Timeout = "bogus"
				})

				It("fails miserably", func() {
					Expect(stepErr).To(MatchError("parse timeout: time: invalid duration \"bogus\""))
				})
			})
		})

		Context("when rootfs uri is set instead of image resource", func() {
			BeforeEach(func() {
				taskPlan.Config.RootfsURI = "some-image"
			})

			It("correctly sets up the image spec", func() {
				Expect(chosenContainer.Spec.ImageSpec).To(Equal(runtime.ImageSpec{
					ImageURL:   "some-image",
					Privileged: false,
				}))
			})
		})

		Context("when tracing is enabled", func() {
			BeforeEach(func() {
				tracing.ConfigureTraceProvider(oteltest.NewTracerProvider())

				spanCtx, buildSpan := tracing.StartSpan(ctx, "build", nil)
				fakeDelegate.StartSpanReturns(spanCtx, buildSpan)

				chosenContainer.ProcessDefs[0].Stub.Do = func(ctx context.Context, _ *runtimetest.Process) error {
					defer GinkgoRecover()
					// Properly propagates span context
					Expect(tracing.FromContext(ctx)).To(Equal(buildSpan))
					return nil
				}
			})

			AfterEach(func() {
				tracing.Configured = false
			})

			It("populates the TRACEPARENT env var", func() {
				Expect(chosenContainer.Spec.Env).To(ContainElement(MatchRegexp(`TRACEPARENT=.+`)))
			})
		})

		Context("when the configuration specifies paths for inputs", func() {
			var inputArtifact *runtimetest.Volume
			var otherInputArtifact *runtimetest.Volume

			BeforeEach(func() {
				inputArtifact = runtimetest.NewVolume("input1")
				otherInputArtifact = runtimetest.NewVolume("input2")

				taskPlan.Config.Inputs = []atc.TaskInputConfig{
					{Name: "some-input", Path: "some-input-configured-path"},
					{Name: "some-other-input"},
				}
			})

			Context("when all inputs are present", func() {
				BeforeEach(func() {
					repo.RegisterArtifact("some-input", inputArtifact)
					repo.RegisterArtifact("some-other-input", otherInputArtifact)
				})

				It("configures the inputs for the containerSpec correctly", func() {
					Expect(chosenContainer.Spec.Inputs).To(ConsistOf([]runtime.Input{
						{
							VolumeHandle:    "input1",
							DestinationPath: "some-artifact-root/some-input-configured-path",
						},
						{
							VolumeHandle:    "input2",
							DestinationPath: "some-artifact-root/some-other-input",
						},
					}))
					Expect(stepErr).ToNot(HaveOccurred())
				})
			})

			Context("when any of the inputs are missing", func() {
				BeforeEach(func() {
					repo.RegisterArtifact("some-input", inputArtifact)
				})

				It("returns a MissingInputsError", func() {
					Expect(stepErr).To(BeAssignableToTypeOf(exec.MissingInputsError{}))
					Expect(stepErr.(exec.MissingInputsError).Inputs).To(ConsistOf("some-other-input"))
				})
			})
		})

		Context("when input is remapped", func() {
			var remappedInputArtifact *runtimetest.Volume

			BeforeEach(func() {
				remappedInputArtifact = runtimetest.NewVolume("input1")
				taskPlan.InputMapping = map[string]string{"remapped-input": "remapped-input-src"}
				taskPlan.Config.Inputs = []atc.TaskInputConfig{
					{Name: "remapped-input"},
				}
			})

			Context("when all inputs are present in the in artifact repository", func() {
				BeforeEach(func() {
					repo.RegisterArtifact("remapped-input-src", remappedInputArtifact)
				})

				It("uses remapped input", func() {
					Expect(chosenContainer.Spec.Inputs).To(ConsistOf([]runtime.Input{
						{
							VolumeHandle:    "input1",
							DestinationPath: "some-artifact-root/remapped-input",
						},
					}))
					Expect(stepErr).ToNot(HaveOccurred())
				})
			})
		})

		Context("when some inputs are optional", func() {
			var (
				optionalInputArtifact, optionalInput2Artifact, requiredInputArtifact *runtimetest.Volume
			)

			BeforeEach(func() {
				optionalInputArtifact = runtimetest.NewVolume("optional1")
				optionalInput2Artifact = runtimetest.NewVolume("optional2")
				requiredInputArtifact = runtimetest.NewVolume("required1")
				taskPlan.Config.Inputs = []atc.TaskInputConfig{
					{Name: "optional-input", Optional: true},
					{Name: "optional-input-2", Optional: true},
					{Name: "required-input"},
				}
			})

			Context("when an optional input is missing", func() {
				BeforeEach(func() {
					repo.RegisterArtifact("required-input", requiredInputArtifact)
					repo.RegisterArtifact("optional-input-2", optionalInput2Artifact)
				})

				It("runs successfully without the optional input", func() {
					Expect(chosenContainer.Spec.Inputs).To(ConsistOf([]runtime.Input{
						{
							VolumeHandle:    "required1",
							DestinationPath: "some-artifact-root/required-input",
						},
						{
							VolumeHandle:    "optional2",
							DestinationPath: "some-artifact-root/optional-input-2",
						},
					}))
					Expect(stepErr).ToNot(HaveOccurred())
				})
			})

			Context("when a required input is missing", func() {
				BeforeEach(func() {
					repo.RegisterArtifact("optional-input", optionalInputArtifact)
					repo.RegisterArtifact("optional-input-2", optionalInput2Artifact)
				})

				It("returns a MissingInputsError", func() {
					Expect(stepErr).To(BeAssignableToTypeOf(exec.MissingInputsError{}))
					Expect(stepErr.(exec.MissingInputsError).Inputs).To(ConsistOf("required-input"))
				})
			})
		})

		Context("when the configuration specifies paths for caches", func() {
			var (
				volume1 *runtimetest.Volume
				volume2 *runtimetest.Volume
			)

			BeforeEach(func() {
				taskPlan.Config.Caches = []atc.TaskCacheConfig{
					{Path: "some-path-1"},
					{Path: "some-path-2"},
				}

				volume1 = runtimetest.NewVolume("volume1")
				volume2 = runtimetest.NewVolume("volume2")

				chosenContainer.Mounts = []runtime.VolumeMount{
					{
						Volume:    volume1,
						MountPath: "some-artifact-root/some-path-1",
					},
					{
						Volume:    volume2,
						MountPath: "some-artifact-root/some-path-2",
					},
				}
			})

			It("creates the containerSpec with the caches", func() {
				Expect(chosenContainer.Spec.Caches).To(ConsistOf("some-path-1", "some-path-2"))
			})

			itRegistersCaches := func(didRegister bool) {
				It("registers cache volumes as task caches", func() {
					Expect(volume1.TaskCacheInitialized).To(Equal(didRegister))
					Expect(volume2.TaskCacheInitialized).To(Equal(didRegister))
				})
			}

			Context("when task belongs to a job", func() {
				BeforeEach(func() {
					stepMetadata.JobID = 12
				})

				Context("when the task succeeds", func() {
					itRegistersCaches(true)
				})

				Context("when the task exits nonzero", func() {
					BeforeEach(func() {
						chosenContainer.ProcessDefs[0].Stub.ExitStatus = 1
					})

					itRegistersCaches(true)
				})

				Context("when the task errors", func() {
					BeforeEach(func() {
						chosenContainer.ProcessDefs[0].Stub.Err = "blah"
					})

					itRegistersCaches(true)
				})
			})

			Context("when task does not belong to job (one-off build)", func() {
				BeforeEach(func() {
					stepMetadata.JobID = 0
				})

				It("does not error", func() {
					Expect(stepErr).ToNot(HaveOccurred())
				})

				itRegistersCaches(false)
			})
		})

		Context("when the configuration specifies paths for outputs", func() {
			var outputVolume1, outputVolume2, outputVolume3 *runtimetest.Volume

			BeforeEach(func() {
				taskPlan.Config.Outputs = []atc.TaskOutputConfig{
					{Name: "some-output", Path: "some-output-configured-path"},
					{Name: "some-other-output"},
					{Name: "some-trailing-slash-output", Path: "some-output-configured-path-with-trailing-slash/"},
				}
				taskPlan.OutputMapping = map[string]string{
					"some-other-output": "some-remapped-output",
				}

				outputVolume1 = runtimetest.NewVolume("output1")
				outputVolume2 = runtimetest.NewVolume("output2")
				outputVolume3 = runtimetest.NewVolume("output3")

				chosenContainer.Mounts = []runtime.VolumeMount{
					{
						Volume:    outputVolume1,
						MountPath: "some-artifact-root/some-output-configured-path/",
					},
					{
						Volume:    outputVolume2,
						MountPath: "some-artifact-root/some-other-output/",
					},
					{
						Volume:    outputVolume3,
						MountPath: "some-artifact-root/some-output-configured-path-with-trailing-slash/",
					},
				}
			})

			It("configures them appropriately in the container spec", func() {
				Expect(chosenContainer.Spec.Outputs).To(Equal(runtime.OutputPaths{
					"some-output":                "some-artifact-root/some-output-configured-path/",
					"some-other-output":          "some-artifact-root/some-other-output/",
					"some-trailing-slash-output": "some-artifact-root/some-output-configured-path-with-trailing-slash/",
				}))
			})

			It("registers the outputs in the build repo", func() {
				Expect(repo.AsMap()).To(Equal(map[build.ArtifactName]runtime.Volume{
					"some-output":                outputVolume1,
					"some-remapped-output":       outputVolume2,
					"some-trailing-slash-output": outputVolume3,
				}))
			})
		})

		Context("when missing the platform", func() {
			BeforeEach(func() {
				taskPlan.Config.Platform = ""
			})

			It("returns the error", func() {
				Expect(stepErr).To(HaveOccurred())
			})

			It("is not successful", func() {
				Expect(stepOk).To(BeFalse())
			})
		})

		Context("when missing the path to the executable", func() {
			BeforeEach(func() {
				taskPlan.Config.Run.Path = ""
			})

			It("returns the error", func() {
				Expect(stepErr).To(HaveOccurred())
			})

			It("is not successful", func() {
				Expect(stepOk).To(BeFalse())
			})
		})

		Context("when an image artifact name is specified", func() {
			BeforeEach(func() {
				taskPlan.ImageArtifactName = "some-image-artifact"
			})

			Context("when the image artifact is registered in the artifact repo", func() {
				var imageVolume *runtimetest.Volume

				BeforeEach(func() {
					imageVolume = runtimetest.NewVolume("image-volume")
					repo.RegisterArtifact("some-image-artifact", imageVolume)
				})

				It("configures it in the containerSpec's ImageSpec", func() {
					Expect(chosenContainer.Spec.ImageSpec).To(Equal(runtime.ImageSpec{
						ImageVolume: "image-volume",
					}))

					expectWorkerSpecResourceTypeUnset()
				})

				Describe("when task config specifies image and/or image resource as well as image artifact", func() {
					Context("when streaming the metadata from the worker succeeds", func() {
						JustBeforeEach(func() {
							Expect(stepErr).ToNot(HaveOccurred())
						})

						Context("when the task config also specifies a rootfs_uri", func() {
							BeforeEach(func() {
								taskPlan.Config.RootfsURI = "some-image"
							})

							It("still uses the image artifact", func() {
								Expect(chosenContainer.Spec.ImageSpec).To(Equal(runtime.ImageSpec{
									ImageVolume: "image-volume",
								}))
								expectWorkerSpecResourceTypeUnset()
							})
						})

						Context("when the task config also specifies image_resource", func() {
							BeforeEach(func() {
								taskPlan.Config.ImageResource = &atc.ImageResource{
									Type:    "docker",
									Source:  atc.Source{"some": "super-secret-source"},
									Params:  atc.Params{"some": "params"},
									Version: atc.Version{"some": "version"},
								}
							})

							It("still uses the image artifact", func() {
								Expect(chosenContainer.Spec.ImageSpec).To(Equal(runtime.ImageSpec{
									ImageVolume: "image-volume",
								}))
								expectWorkerSpecResourceTypeUnset()
							})
						})
					})
				})
			})

			Context("when the image artifact is NOT registered in the artifact repo", func() {
				It("returns a MissingTaskImageSourceError", func() {
					Expect(stepErr).To(Equal(exec.MissingTaskImageSourceError{"some-image-artifact"}))
				})

				It("is not successful", func() {
					Expect(stepOk).To(BeFalse())
				})
			})
		})

		Context("when the image_resource is specified (even if rootfs_uri is configured)", func() {
			var fetchedImageSpec runtime.ImageSpec

			BeforeEach(func() {
				taskPlan.Config.RootfsURI = "some-image"
				taskPlan.Config.ImageResource = &atc.ImageResource{
					Type:   "docker",
					Source: atc.Source{"some": "super-secret-source"},
					Params: atc.Params{"some": "params"},
				}

				fetchedImageSpec = runtime.ImageSpec{
					ImageVolume: "some-volume",
				}

				fakeDelegate.FetchImageReturns(fetchedImageSpec, nil)
			})

			It("succeeds", func() {
				Expect(stepErr).ToNot(HaveOccurred())
				Expect(stepOk).To(BeTrue())
			})

			It("fetches the image", func() {
				Expect(fakeDelegate.FetchImageCallCount()).To(Equal(1))
				_, imageResource, types, privileged := fakeDelegate.FetchImageArgsForCall(0)
				Expect(imageResource).To(Equal(atc.ImageResource{
					Type:   "docker",
					Source: atc.Source{"some": "super-secret-source"},
					Params: atc.Params{"some": "params"},
				}))
				Expect(types).To(Equal(taskPlan.VersionedResourceTypes))
				Expect(privileged).To(BeFalse())
			})

			It("creates the specs with the fetched image", func() {
				Expect(chosenContainer.Spec.ImageSpec).To(Equal(fetchedImageSpec))
			})

			Context("when tags are specified on the task plan", func() {
				BeforeEach(func() {
					taskPlan.Tags = atc.Tags{"plan", "tags"}
				})

				It("fetches the image with the same tags", func() {
					Expect(fakeDelegate.FetchImageCallCount()).To(Equal(1))
					_, imageResource, _, _ := fakeDelegate.FetchImageArgsForCall(0)
					Expect(imageResource.Tags).To(Equal(atc.Tags{"plan", "tags"}))
				})
			})

			Context("when tags are specified on the image resource", func() {
				BeforeEach(func() {
					taskPlan.Config.ImageResource.Tags = atc.Tags{"image", "tags"}
				})

				It("fetches the image with the same tags", func() {
					Expect(fakeDelegate.FetchImageCallCount()).To(Equal(1))
					_, imageResource, _, _ := fakeDelegate.FetchImageArgsForCall(0)
					Expect(imageResource.Tags).To(Equal(atc.Tags{"image", "tags"}))
				})

				Context("when tags are ALSO specified on the task plan", func() {
					BeforeEach(func() {
						taskPlan.Tags = atc.Tags{"plan", "tags"}
					})

					It("fetches the image using only the image tags", func() {
						Expect(fakeDelegate.FetchImageCallCount()).To(Equal(1))
						_, imageResource, _, _ := fakeDelegate.FetchImageArgsForCall(0)
						Expect(imageResource.Tags).To(Equal(atc.Tags{"image", "tags"}))
					})
				})
			})

			Context("when privileged", func() {
				BeforeEach(func() {
					taskPlan.Privileged = true
				})

				It("fetches a privileged image", func() {
					Expect(fakeDelegate.FetchImageCallCount()).To(Equal(1))
					_, _, _, privileged := fakeDelegate.FetchImageArgsForCall(0)
					Expect(privileged).To(BeTrue())
				})
			})
		})

		Context("when a run dir and user are specified", func() {
			BeforeEach(func() {
				taskPlan.Config.Run.Dir = "/some/dir"
				taskPlan.Config.Run.User = "some-user"

				chosenContainer.ProcessDefs[0].Spec.Dir = "/some/dir"
				chosenContainer.ProcessDefs[0].Spec.User = "some-user"
			})

			It("runs with the specified dir and user", func() {
				Expect(stepErr).ToNot(HaveOccurred())
				Expect(chosenContainer.Processes).To(HaveLen(1))
			})
		})

		Context("when running the task exits with a non-zero status", func() {
			BeforeEach(func() {
				chosenContainer.ProcessDefs[0].Stub.ExitStatus = 1
			})

			It("doesn't error", func() {
				Expect(stepErr).ToNot(HaveOccurred())
			})

			It("is not successful", func() {
				Expect(stepOk).To(BeFalse())
			})

			It("finishes the step", func() {
				Expect(fakeDelegate.FinishedCallCount()).To(Equal(1))
				_, status := fakeDelegate.FinishedArgsForCall(0)
				Expect(status).To(Equal(exec.ExitStatus(1)))
			})
		})

		Context("when running the task fails", func() {
			BeforeEach(func() {
				chosenContainer.ProcessDefs[0].Stub.Err = "failed to run the task"
			})

			It("returns the error", func() {
				Expect(stepErr).To(HaveOccurred())
			})

			It("is not successful", func() {
				Expect(stepOk).To(BeFalse())
			})
		})

		Context("when the task step is interrupted", func() {
			BeforeEach(func() {
				cancel()
				chosenContainer.ProcessDefs[0].Stub.Do = func(ctx context.Context, _ *runtimetest.Process) error {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(1 * time.Second):
						Fail("didn't return context error")
						panic("unreachable")
					}
				}
			})

			It("returns the context.Canceled error", func() {
				Expect(stepErr).To(Equal(context.Canceled))
			})

			It("is not successful", func() {
				Expect(stepOk).To(BeFalse())
			})

			It("doesn't register a artifact", func() {
				Expect(repo.AsMap()).To(BeEmpty())
			})
		})
	})
})
