// one job per arch (for now) that triggers builds for all unbuilt images
properties([
	disableConcurrentBuilds(),
	disableResume(),
	durabilityHint('PERFORMANCE_OPTIMIZED'),
	pipelineTriggers([
		upstream(threshold: 'FAILURE', upstreamProjects: 'meta'),
	]),
])

env.BASHBREW_ARCH = env.JOB_NAME.split('[/-]')[-1] // "gha", "arm64v8", etc

node {
	stage('Checkout') {
		checkout(scmGit(
			userRemoteConfigs: [[
				url: 'git@github.com:docker-library/meta.git',
				credentialsId: 'docker-library-bot',
				name: 'origin',
			]],
			branches: [[name: '*/subset']], // TODO back to main
			extensions: [
				submodule(
					parentCredentials: true,
					recursiveSubmodules: true,
					trackingSubmodules: true,
				),
				cleanBeforeCheckout(),
				cleanAfterCheckout(),
				[$class: 'RelativeTargetDirectory', relativeTargetDir: 'meta'],
			],
		))
	}

	dir('meta') {
		def queue = ''
		stage('Queue') {
			// TODO this job should export a JSON file that includes the number of attempts so far per failing buildId, and then this list should inject those values, initialize missing to 0, and sort by attempts so that failing builds always live at the bottom of the queue
			queue = sh(returnStdout: true, script: '''
				jq -L.scripts '
					include "meta";
					[
						.[]
						| select(
							needs_build
							and (
								.build.arch as $arch
								| if env.BASHBREW_ARCH == "gha" then
									[ "amd64", "i386", "windows-amd64" ]
								else [ env.BASHBREW_ARCH ] end
								| index($arch)
							)
						)
					]
				' builds.json
			''').trim()
		}
		if (queue) {
			queue = readJSON(text: queue)
			currentBuild.displayName = 'queue size: ' + queue.size() + ' (#' + currentBuild.number + ')'
		} else {
			currentBuild.displayName = 'empty queue (#' + currentBuild.number + ')'
			return
		}

		for (buildObj in queue) {
			def identifier = buildObj.source.allTags[0]
			if (env.BASHBREW_ARCH == 'gha') {
				identifier += ' (' + buildObj.build.arch + ')'
			}
			def json = writeJSON(json: buildObj, returnText: true)
			withEnv([
				'json=' + json,
			]) {
				stage(identifier) {
					sh '''#!/usr/bin/env bash
						set -Eeuo pipefail -x
						jq <<<"$json" .
					'''
					if (env.BASHBREW_ARCH == 'gha') {
						// TODO
						echo('GHA')
					} else {
						def res = build(
							job: 'build-' + env.BASHBREW_ARCH,
							parameters: [
								string(name: 'buildId', value: buildObj.buildId),
							],
							propagate: false,
							quietPeriod: 5, // seconds
						)
						// TODO do something useful with "res.result" (especially "res.result != 'SUCCESS'")
						// (maybe store "res.startTimeInMillis + res.duration" as endTime so we can implement some amount of backoff somehow?)
						echo(res.result)
					}
				}
			}
		}
	}
}
